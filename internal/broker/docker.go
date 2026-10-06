// SPDX-License-Identifier: MIT

package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// apiVersion is the Docker Engine API the broker speaks. 1.45 is Docker 26.1,
// what the homelab runs; newer daemons still serve it.
const apiVersion = "v1.45"

// containerPrefix names every browser this broker owns. The reaper and the
// admin page find them by the label, not by this prefix; the prefix is for
// people reading `docker ps`.
const containerPrefix = "kiosk-browser-"

// userLabel marks a container as a kiosk browser and records whose it is.
const userLabel = "kiosk.user"

// ErrNotFound means the container (or image) does not exist.
var ErrNotFound = errors.New("not found")

// BrowserSpec is everything that varies between deployments. Everything else
// about the container is fixed in Create, on purpose: the socket proxy cannot
// look inside a create request, so this code is the only thing that keeps a
// browser from being created privileged or with the host mounted.
type BrowserSpec struct {
	Image       string // pinned by digest
	Network     string // the edge network, so the broker reaches it by name
	StagingBase string // host path; also mounted at the same path in the broker
	StartURL    string
	TZ          string
	MemoryMB    int // 0 = no limit
}

// Docker talks to the Engine API through tecnativa/docker-socket-proxy, never
// the bare socket. It is a few plain HTTP calls: the official SDK would be the
// largest thing in the binary for five endpoints.
type Docker struct {
	base   string // e.g. http://kiosk-socket-proxy:2375
	client *http.Client
	// pulls stream progress for minutes; they are bounded by their context,
	// not by the short timeout of every other call.
	pullClient *http.Client
}

// NewDocker builds a client for the socket proxy at base.
func NewDocker(base string) *Docker {
	return &Docker{base: base, client: &http.Client{Timeout: 30 * time.Second}, pullClient: &http.Client{}}
}

// ContainerName is the browser container for a user. user must already be a
// SafeName, which Identify guarantees.
func ContainerName(user string) string { return containerPrefix + user }

// Container is what the broker needs to know about a browser.
type Container struct {
	Name    string
	User    string
	Running bool
	Created time.Time
}

// Inspect returns the user's browser, or ErrNotFound.
func (d *Docker) Inspect(ctx context.Context, user string) (Container, error) {
	var out struct {
		Name    string
		Created time.Time
		State   struct{ Running bool }
		Config  struct{ Labels map[string]string }
	}
	if err := d.do(ctx, http.MethodGet, "/containers/"+ContainerName(user)+"/json", nil, &out); err != nil {
		return Container{}, err
	}
	// A container that merely has the name but not our label is not ours to
	// route to or destroy.
	if out.Config.Labels[userLabel] != user {
		return Container{}, fmt.Errorf("container %s exists but is not a kiosk browser for %q", ContainerName(user), user)
	}
	return Container{Name: ContainerName(user), User: user, Running: out.State.Running, Created: out.Created}, nil
}

// List returns every kiosk browser, running or not.
func (d *Docker) List(ctx context.Context) ([]Container, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {userLabel}})
	var out []struct {
		Names   []string
		Created int64
		State   string
		Labels  map[string]string
	}
	if err := d.do(ctx, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), nil, &out); err != nil {
		return nil, err
	}
	cs := make([]Container, 0, len(out))
	for _, c := range out {
		user := c.Labels[userLabel]
		cs = append(cs, Container{Name: ContainerName(user), User: user, Running: c.State == "running", Created: time.Unix(c.Created, 0)})
	}
	return cs, nil
}

// Create makes and starts the user's browser. The downloads directory is
// created first, by the broker, which runs as uid 65532: left to Docker, a
// missing bind source would be created owned by root and zlatan could not
// clean it up.
func (d *Docker) Create(ctx context.Context, spec BrowserSpec, user string) error {
	dl := path.Join(spec.StagingBase, user)
	if err := os.MkdirAll(dl, 0o750); err != nil {
		return fmt.Errorf("create downloads dir: %w", err)
	}
	body := map[string]any{
		"Image":  spec.Image,
		"Labels": map[string]string{userLabel: user},
		"Env": []string{
			"TZ=" + spec.TZ,
			// zlatan's uid: the downloads are its to read and purge.
			"PUID=65532",
			"PGID=65532",
			"CHROME_CLI=" + spec.StartURL,
		},
		"HostConfig": map[string]any{
			// Only the downloads persist. /config (profile, cookies) stays in
			// the container layer and dies with `docker rm`.
			"Binds":         []string{dl + ":/config/Downloads"},
			"NetworkMode":   spec.Network,
			"ShmSize":       int64(1 << 30),
			"SecurityOpt":   []string{"seccomp:unconfined"},
			"RestartPolicy": map[string]string{"Name": "no"},
			"Memory":        int64(spec.MemoryMB) << 20,
		},
	}
	name := ContainerName(user)
	if err := d.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), body, nil); err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	if err := d.do(ctx, http.MethodPost, "/containers/"+name+"/start", nil, nil); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	return nil
}

// Pull fetches ref (a tag or a digest) and returns it pinned by digest, which
// is what gets stored and run: a tag can move under you, a digest cannot.
func (d *Docker) Pull(ctx context.Context, ref string) (string, error) {
	repo, tag := splitRef(ref)
	q := url.Values{"fromImage": {repo}, "tag": {tag}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base+"/"+apiVersion+"/images/create?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := d.pullClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("pull %s: %s: %s", ref, resp.Status, bytes.TrimSpace(msg))
	}
	// A 200 only means the pull started; failures arrive in the stream.
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct{ Error string }
		if err := dec.Decode(&msg); err == io.EOF {
			break
		} else if err != nil {
			return "", fmt.Errorf("pull %s: %w", ref, err)
		}
		if msg.Error != "" {
			return "", fmt.Errorf("pull %s: %s", ref, msg.Error)
		}
	}
	name := repo + ":" + tag
	if strings.HasPrefix(tag, "sha256:") {
		name = repo + "@" + tag
	}
	var img struct{ RepoDigests []string }
	if err := d.do(ctx, http.MethodGet, "/images/"+name+"/json", nil, &img); err != nil {
		return "", err
	}
	for _, rd := range img.RepoDigests {
		if strings.HasPrefix(rd, repo+"@") {
			return rd, nil
		}
	}
	return "", fmt.Errorf("pull %s: no digest for %s among %v", ref, repo, img.RepoDigests)
}

// splitRef splits an image reference into repository and tag-or-digest. A
// bare name means "latest", which Pull then pins by digest.
func splitRef(ref string) (repo, tag string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// Remove destroys the user's browser, profile and cookies with it. Removing
// one that is already gone is not an error.
func (d *Docker) Remove(ctx context.Context, user string) error {
	err := d.do(ctx, http.MethodDelete, "/containers/"+ContainerName(user)+"?force=1&v=1", nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (d *Docker) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base+"/"+apiVersion+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("docker %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
