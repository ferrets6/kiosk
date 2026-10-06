// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const secret = "0123456789abcdef0123456789abcdef"

func edge(t *testing.T) []*net.IPNet {
	_, n, err := net.ParseCIDR("172.18.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	return []*net.IPNet{n}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"federico-sso": "federico-sso",
		"a.b":          "a_b",
		"../etc":       "etc",
		"__x__":        "x",
		"ü":            "",
		"A_9-z":        "A_9-z",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIdentify(t *testing.T) {
	g := NewGate(edge(t), secret)
	tests := []struct {
		name      string
		remote    string
		secret    string
		user      string
		groups    string
		wantOK    bool
		wantAdmin bool
	}{
		{"ok", "172.18.0.5:1234", secret, "federico-sso", "", true, false},
		{"admin", "172.18.0.5:1234", secret, "marco-sso", "family, lldap_admin", true, true},
		{"admin is exact", "172.18.0.5:1234", secret, "x", "lldap_admins", true, false},
		{"outside edge", "192.168.1.10:1234", secret, "federico-sso", "", false, false},
		{"no secret", "172.18.0.5:1234", "", "federico-sso", "", false, false},
		{"wrong secret", "172.18.0.5:1234", "nope", "federico-sso", "", false, false},
		{"no user", "172.18.0.5:1234", secret, "", "", false, false},
		{"unsafe user collides", "172.18.0.5:1234", secret, "a.b", "", false, false},
		{"traversal", "172.18.0.5:1234", secret, "../x", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remote
			r.Header.Set("X-Kiosk-Proxy-Secret", tt.secret)
			r.Header.Set("Remote-User", tt.user)
			r.Header.Set("Remote-Groups", tt.groups)
			id, ok := g.Identify(r)
			if ok != tt.wantOK || id.Admin != tt.wantAdmin || (ok && id.User != tt.user) {
				t.Fatalf("got %+v %v, want ok=%v admin=%v", id, ok, tt.wantOK, tt.wantAdmin)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	base := map[string]string{
		"KIOSK_TRUSTED_PROXY": "172.18.0.0/16",
		"KIOSK_PROXY_SECRET":  secret,
		"KIOSK_BROWSER_IMAGE": "lscr.io/linuxserver/chromium@sha256:abc",
		"KIOSK_STAGING_BASE":  "/tank/media/staging/zlatan",
	}
	tests := []struct {
		name    string
		set     map[string]string
		wantErr string
	}{
		{"ok", nil, ""},
		{"missing secret", map[string]string{"KIOSK_PROXY_SECRET": ""}, "KIOSK_PROXY_SECRET is required"},
		{"short secret", map[string]string{"KIOSK_PROXY_SECRET": "x"}, "too short"},
		{"tag not digest", map[string]string{"KIOSK_BROWSER_IMAGE": "lscr.io/linuxserver/chromium:latest"}, "pinned by digest"},
		{"bad cidr", map[string]string{"KIOSK_TRUSTED_PROXY": "edge"}, "not a CIDR"},
		{"relative staging", map[string]string{"KIOSK_STAGING_BASE": "staging"}, "absolute"},
		{"stall too long", map[string]string{"KIOSK_DOWNLOAD_STALE": "48h"}, "KIOSK_DOWNLOAD_STALE"},
		{"idle garbage", map[string]string{"KIOSK_VIEWER_IDLE": "soon"}, "KIOSK_VIEWER_IDLE"},
		{"memory garbage", map[string]string{"KIOSK_BROWSER_MEMORY_MB": "2g"}, "KIOSK_BROWSER_MEMORY_MB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			for k, v := range tt.set {
				env[k] = v
			}
			cfg, err := Load(func(k string) string { return env[k] })
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if tt.wantErr == "" && (cfg.Defaults != Settings{StallMin: 10, ViewerIdleMin: 15, MemoryMB: 2048}) {
				t.Fatalf("defaults = %+v", cfg.Defaults)
			}
		})
	}
}

func TestSplitRef(t *testing.T) {
	for in, want := range map[string][2]string{
		"lscr.io/linuxserver/chromium:1.2":      {"lscr.io/linuxserver/chromium", "1.2"},
		"lscr.io/linuxserver/chromium":          {"lscr.io/linuxserver/chromium", "latest"},
		"lscr.io/linuxserver/chromium@sha256:a": {"lscr.io/linuxserver/chromium", "sha256:a"},
		"registry:5000/chromium":                {"registry:5000/chromium", "latest"},
		"registry:5000/chromium:x":              {"registry:5000/chromium", "x"},
	} {
		if r, tag := splitRef(in); r != want[0] || tag != want[1] {
			t.Errorf("splitRef(%q) = %q %q, want %v", in, r, tag, want)
		}
	}
}

func TestReapable(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	set := Settings{StallMin: 10, ViewerIdleMin: 15}
	ago := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	tests := []struct {
		name string
		a    activity
		d    Downloads
		want bool
	}{
		{"idle and still", activity{lastSeen: ago(20)}, Downloads{LastWrite: ago(30)}, true},
		{"never downloaded", activity{lastSeen: ago(20)}, Downloads{}, true},
		{"websocket open", activity{lastSeen: ago(60), viewers: 1}, Downloads{}, false},
		{"recently seen", activity{lastSeen: ago(5)}, Downloads{}, false},
		{"tab closed, downloading", activity{lastSeen: ago(60)}, Downloads{InProgress: true, LastWrite: ago(30)}, false},
		{"tab closed, file just written", activity{lastSeen: ago(60)}, Downloads{LastWrite: ago(3)}, false},
		{"starting", activity{lastSeen: ago(60), starting: true}, Downloads{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reapable(now, tt.a, tt.d, set); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScanDownloads(t *testing.T) {
	dir := t.TempDir()
	if d := scanDownloads(filepath.Join(dir, "missing")); d.InProgress || !d.LastWrite.IsZero() {
		t.Fatalf("missing dir: %+v", d)
	}
	old := time.Now().Add(-time.Hour)
	os.WriteFile(filepath.Join(dir, "takeout-1.zip"), nil, 0o600)
	os.Chtimes(filepath.Join(dir, "takeout-1.zip"), old, old)
	if d := scanDownloads(dir); d.InProgress || !d.LastWrite.Equal(old) {
		t.Fatalf("finished file: %+v", d)
	}
	os.WriteFile(filepath.Join(dir, "takeout-2.zip.crdownload"), nil, 0o600)
	if d := scanDownloads(dir); !d.InProgress {
		t.Fatalf("crdownload not seen: %+v", d)
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.SetImage("x@sha256:1")
	st.SetSettings("federico-sso", Settings{StallMin: 60})
	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Image() != "x@sha256:1" || st2.Settings("federico-sso").StallMin != 60 {
		t.Fatalf("not persisted: %q %+v", st2.Image(), st2.Settings("federico-sso"))
	}
	st2.SetSettings("federico-sso", Settings{})
	if len(st2.Users()) != 0 {
		t.Fatal("empty settings should drop the user")
	}
}

// fakeDocker records what the broker asks of the Engine API and holds the
// containers it would have.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]bool // name -> running
	calls      []string
	created    map[string]any
	pullErr    string
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/"+apiVersion)
	f.calls = append(f.calls, r.Method+" "+path)
	name := strings.Split(strings.TrimPrefix(path, "/containers/"), "/")[0]
	switch {
	case r.Method == "POST" && path == "/containers/create":
		json.NewDecoder(r.Body).Decode(&f.created)
		f.containers[r.URL.Query().Get("name")] = false
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"Id":"x"}`))
	case r.Method == "POST" && strings.HasSuffix(path, "/start"):
		f.containers[name] = true
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "POST" && path == "/images/create":
		w.Write([]byte(`{"status":"Pulling"}` + "\n"))
		if f.pullErr != "" {
			w.Write([]byte(`{"error":"` + f.pullErr + `"}` + "\n"))
		}
	case r.Method == "GET" && strings.HasPrefix(path, "/images/"):
		repo := strings.Split(strings.TrimPrefix(path, "/images/"), ":")[0]
		json.NewEncoder(w).Encode(map[string]any{"RepoDigests": []string{"other@sha256:0", repo + "@sha256:feed"}})
	case r.Method == "GET" && path == "/containers/json":
		var out []map[string]any
		for n, running := range f.containers {
			state := "exited"
			if running {
				state = "running"
			}
			out = append(out, map[string]any{"Names": []string{"/" + n}, "State": state, "Labels": map[string]string{userLabel: strings.TrimPrefix(n, containerPrefix)}})
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == "GET" && strings.HasSuffix(path, "/json"):
		running, ok := f.containers[name]
		if !ok {
			http.Error(w, "no such container", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"State":  map[string]bool{"Running": running},
			"Config": map[string]any{"Labels": map[string]string{userLabel: strings.TrimPrefix(name, containerPrefix)}},
		})
	case r.Method == "DELETE":
		if _, ok := f.containers[name]; !ok {
			http.Error(w, "no such container", http.StatusNotFound)
			return
		}
		delete(f.containers, name)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected", http.StatusTeapot)
	}
}

func (f *fakeDocker) has(name string) (running, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	running, ok = f.containers[name]
	return
}

// upstream stands in for every user's browser UI and records what reached it.
type upstream struct {
	mu   sync.Mutex
	seen http.Header
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.seen = r.Header.Clone()
	u.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, "<html><body>selkies "+r.Host+"</body></html>")
}

func newTestServer(t *testing.T) (*Server, *fakeDocker, *upstream) {
	f := &fakeDocker{containers: map[string]bool{}}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	up := &upstream{}
	us := httptest.NewServer(up)
	t.Cleanup(us.Close)
	cfg := Config{
		TrustedProxy: edge(t),
		ProxySecret:  secret,
		DockerHost:   ts.URL,
		Browser: BrowserSpec{
			Image:       "chromium@sha256:abc",
			Network:     "edge",
			StagingBase: filepath.ToSlash(t.TempDir()),
			StartURL:    "https://takeout.google.com/",
			TZ:          "Europe/Rome",
		},
		Defaults: Settings{StallMin: 10, ViewerIdleMin: 15, MemoryMB: 2048},
	}
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg, st)
	target, _ := url.Parse(us.URL)
	s.target = func(string) *url.URL { return target }
	return s, f, up
}

func request(s *Server, method, path, user string, form url.Values, hdr ...string) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.RemoteAddr = "172.18.0.2:5555"
	r.Header.Set("X-Kiosk-Proxy-Secret", secret)
	r.Header.Set("Remote-User", user)
	r.Header.Set("Accept", "text/html")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func TestLifecycle(t *testing.T) {
	s, f, up := newTestServer(t)

	if w := request(s, "GET", "/", "federico-sso", nil); !strings.Contains(w.Body.String(), "Sto preparando") {
		t.Fatalf("first visit should wait: %d %q", w.Code, w.Body)
	}
	waitFor(t, func() bool { running, _ := f.has("kiosk-browser-federico-sso"); return running })

	// The spec is fixed by the broker; nothing in it comes from the request.
	f.mu.Lock()
	hc := f.created["HostConfig"].(map[string]any)
	f.mu.Unlock()
	if b := hc["Binds"].([]any); len(b) != 1 || !strings.HasSuffix(b[0].(string), "/federico-sso:/config/Downloads") {
		t.Errorf("binds = %v", b)
	}
	if _, ok := hc["Privileged"]; ok {
		t.Error("browser must never be privileged")
	}
	if hc["Memory"].(float64) != 2048<<20 {
		t.Errorf("memory = %v", hc["Memory"])
	}

	waitFor(t, func() bool {
		return strings.Contains(request(s, "GET", "/_kiosk/ready", "federico-sso", nil).Body.String(), `"ready":true`)
	})

	w := request(s, "GET", "/", "federico-sso", nil)
	if !strings.Contains(w.Body.String(), "selkies") || !strings.Contains(w.Body.String(), overlayTag+"</body>") {
		t.Fatalf("not proxied with overlay: %q", w.Body)
	}
	up.mu.Lock()
	for _, h := range []string{"X-Kiosk-Proxy-Secret", "Remote-User", "Cookie"} {
		if up.seen.Get(h) != "" {
			t.Errorf("%s leaked to the browser container", h)
		}
	}
	up.mu.Unlock()

	// Another person gets their own container, never the first one.
	request(s, "GET", "/", "marco-sso", nil)
	waitFor(t, func() bool { running, _ := f.has("kiosk-browser-marco-sso"); return running })

	if w := request(s, "POST", "/_kiosk/done", "federico-sso", nil); w.Code != http.StatusNoContent {
		t.Fatalf("done: %d", w.Code)
	}
	if _, ok := f.has("kiosk-browser-federico-sso"); ok {
		t.Fatal("container survived done")
	}
	if running, _ := f.has("kiosk-browser-marco-sso"); !running {
		t.Fatal("done destroyed somebody else's browser")
	}
	if w := request(s, "POST", "/_kiosk/done", "federico-sso", nil); w.Code != http.StatusNoContent {
		t.Fatalf("second done: %d", w.Code)
	}
}

func TestCrossSitePostRefused(t *testing.T) {
	s, f, _ := newTestServer(t)
	f.mu.Lock()
	f.containers["kiosk-browser-federico-sso"] = true
	f.mu.Unlock()
	if w := request(s, "POST", "/_kiosk/done", "federico-sso", nil, "Sec-Fetch-Site", "cross-site"); w.Code != http.StatusForbidden {
		t.Fatalf("got %d", w.Code)
	}
	if _, ok := f.has("kiosk-browser-federico-sso"); !ok {
		t.Fatal("cross-site POST destroyed the browser")
	}
}

func TestUnauthenticatedGetsNothing(t *testing.T) {
	s, f, _ := newTestServer(t)
	for _, p := range []string{"/", "/websocket", "/_kiosk/ready", "/admin"} {
		if w := request(s, "GET", p, "", nil); w.Code != http.StatusForbidden {
			t.Fatalf("%s: got %d", p, w.Code)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("docker was called: %v", f.calls)
	}
}

func TestReapOnce(t *testing.T) {
	s, f, _ := newTestServer(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	for _, u := range []string{"idle", "watching", "downloading", "stopped"} {
		f.containers["kiosk-browser-"+u] = u != "stopped"
		s.activity[u] = &activity{lastSeen: now.Add(-time.Hour)}
	}
	s.activity["watching"].viewers = 1
	dl := filepath.Join(s.cfg.Browser.StagingBase, "downloading")
	os.MkdirAll(dl, 0o750)
	os.WriteFile(filepath.Join(dl, "x.zip.crdownload"), nil, 0o600)

	s.reapOnce(context.Background())
	for u, want := range map[string]bool{"idle": false, "watching": true, "downloading": true, "stopped": false} {
		if _, ok := f.has("kiosk-browser-" + u); ok != want {
			t.Errorf("%s: alive=%v, want %v", u, ok, want)
		}
	}
}

func TestUserSettings(t *testing.T) {
	s, _, _ := newTestServer(t)
	if w := request(s, "POST", "/_kiosk/settings", "federico-sso", url.Values{"stall_minutes": {"60"}}); w.Code != http.StatusNoContent {
		t.Fatalf("choice refused: %d", w.Code)
	}
	if w := request(s, "POST", "/_kiosk/settings", "federico-sso", url.Values{"stall_minutes": {"100000"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("out-of-list value accepted: %d", w.Code)
	}
	if got := s.settings("federico-sso"); got.StallMin != 60 || got.ViewerIdleMin != 15 {
		t.Fatalf("settings = %+v", got)
	}
}

func TestAdmin(t *testing.T) {
	s, f, _ := newTestServer(t)
	admin := []string{"Remote-Groups", "lldap_admin"}
	if w := request(s, "GET", "/admin", "federico-sso", nil); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin got %d", w.Code)
	}
	if w := request(s, "POST", "/admin/destroy", "federico-sso", url.Values{"user": {"marco-sso"}}); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin destroy got %d", w.Code)
	}

	f.mu.Lock()
	f.containers["kiosk-browser-marco-sso"] = true
	f.mu.Unlock()
	if w := request(s, "GET", "/admin", "federico-sso", nil, admin...); w.Code != 200 || !strings.Contains(w.Body.String(), "marco-sso") {
		t.Fatalf("admin page: %d %q", w.Code, w.Body)
	}

	w := request(s, "POST", "/admin/settings", "federico-sso", url.Values{"user": {"marco-sso"}, "stall_minutes": {"120"}, "memory_mb": {"4096"}}, admin...)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("settings: %d", w.Code)
	}
	if got := s.settings("marco-sso"); got != (Settings{StallMin: 120, ViewerIdleMin: 15, MemoryMB: 4096}) {
		t.Fatalf("settings = %+v", got)
	}
	request(s, "POST", "/admin/settings", "federico-sso", url.Values{"user": {"marco-sso"}, "memory_mb": {"1"}}, admin...)
	if s.settings("marco-sso").MemoryMB != 4096 {
		t.Fatal("invalid memory accepted")
	}
	if w := request(s, "POST", "/admin/settings", "federico-sso", url.Values{"user": {"../x"}}, admin...); w.Code != http.StatusBadRequest {
		t.Fatalf("bad user: %d", w.Code)
	}

	request(s, "POST", "/admin/destroy", "federico-sso", url.Values{"user": {"marco-sso"}}, admin...)
	if _, ok := f.has("kiosk-browser-marco-sso"); ok {
		t.Fatal("admin destroy did nothing")
	}

	request(s, "POST", "/admin/image", "federico-sso", url.Values{"ref": {"lscr.io/linuxserver/chromium:latest"}}, admin...)
	if got := s.store.Image(); got != "lscr.io/linuxserver/chromium@sha256:feed" {
		t.Fatalf("image = %q", got)
	}
	f.mu.Lock()
	f.pullErr = "manifest unknown"
	f.mu.Unlock()
	w = request(s, "POST", "/admin/image", "federico-sso", url.Values{"ref": {"lscr.io/linuxserver/chromium:nope"}}, admin...)
	if !strings.Contains(w.Header().Get("Location"), "manifest+unknown") || s.store.Image() != "lscr.io/linuxserver/chromium@sha256:feed" {
		t.Fatalf("failed pull changed the image or hid the error: %q", w.Header().Get("Location"))
	}
}

func TestAdminDefaultsAndUsers(t *testing.T) {
	s, f, _ := newTestServer(t)
	admin := []string{"Remote-Groups", "lldap_admin"}

	// marco used kiosk once and his browser is gone; federico has one now.
	request(s, "GET", "/", "marco-sso", nil)
	waitFor(t, func() bool { return !s.store.LastUsed("marco-sso").IsZero() })
	request(s, "POST", "/_kiosk/done", "marco-sso", nil)
	f.mu.Lock()
	f.containers["kiosk-browser-federico-sso"] = true
	f.mu.Unlock()

	body := request(s, "GET", "/admin", "federico-sso", nil, admin...).Body.String()
	for _, want := range []string{`value="marco-sso"`, `value="federico-sso"`, `action="/admin/destroy"`} {
		if !strings.Contains(body, want) {
			t.Errorf("admin page lacks %q", want)
		}
	}

	request(s, "POST", "/admin/defaults", "federico-sso", url.Values{"stall_minutes": {"30"}, "viewer_idle_minutes": {"20"}}, admin...)
	request(s, "POST", "/admin/settings", "federico-sso", url.Values{"user": {"marco-sso"}, "stall_minutes": {"5"}}, admin...)
	if got := s.settings("federico-sso"); got != (Settings{StallMin: 30, ViewerIdleMin: 20, MemoryMB: 2048}) {
		t.Fatalf("defaults not applied: %+v", got)
	}
	if got := s.settings("marco-sso"); got != (Settings{StallMin: 5, ViewerIdleMin: 20, MemoryMB: 2048}) {
		t.Fatalf("override over defaults: %+v", got)
	}
	w := request(s, "POST", "/admin/defaults", "federico-sso", url.Values{"stall_minutes": {"0"}}, admin...)
	if !strings.Contains(w.Header().Get("Location"), "non+validi") || s.settings("federico-sso").StallMin != 30 {
		t.Fatalf("invalid default accepted: %q", w.Header().Get("Location"))
	}
}
