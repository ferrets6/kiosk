// SPDX-License-Identifier: MIT

package broker

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"date": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Local().Format("2006-01-02 15:04")
	},
}).ParseFS(webFS, "web/*.html"))

// Server is the broker's HTTP surface.
type Server struct {
	cfg    Config
	gate   *Gate
	docker *Docker
	store  *Store
	now    func() time.Time
	// target is where a user's browser UI listens. Tests point it elsewhere.
	target func(user string) *url.URL

	mu       sync.Mutex
	activity map[string]*activity
}

// NewServer wires a server from its configuration.
func NewServer(cfg Config, store *Store) *Server {
	return &Server{
		cfg:    cfg,
		gate:   NewGate(cfg.TrustedProxy, cfg.ProxySecret),
		docker: NewDocker(cfg.DockerHost),
		store:  store,
		now:    time.Now,
		target: func(user string) *url.URL {
			return &url.URL{Scheme: "http", Host: ContainerName(user) + ":3000"}
		},
		activity: map[string]*activity{},
	}
}

// Handler returns the routes. The broker owns /admin and /_kiosk/; every other
// path belongs to the caller's own browser and is proxied there.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_kiosk/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /_kiosk/overlay.js", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, webFS, "web/overlay.js")
	})
	mux.Handle("GET /_kiosk/ready", s.authed(s.ready))
	mux.Handle("POST /_kiosk/done", s.authed(s.done))
	mux.Handle("GET /_kiosk/settings", s.authed(s.getSettings))
	mux.Handle("POST /_kiosk/settings", s.authed(s.setSettings))
	mux.Handle("/_kiosk/", http.NotFoundHandler())
	mux.Handle("GET /admin", s.admin(s.adminPage))
	mux.Handle("POST /admin/destroy", s.admin(s.adminDestroy))
	mux.Handle("POST /admin/settings", s.admin(s.adminSettings))
	mux.Handle("POST /admin/defaults", s.admin(s.adminDefaults))
	mux.Handle("POST /admin/image", s.admin(s.adminImage))
	mux.Handle("/admin/", http.NotFoundHandler())
	mux.Handle("/", s.authed(s.browser))
	return mux
}

// authed resolves the caller and refuses cross-site writes: every POST here
// destroys or changes something, and must come from kiosk's own pages.
func (s *Server) authed(next func(http.ResponseWriter, *http.Request, Identity)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.gate.Identify(r)
		if !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		next(w, r, id)
	})
}

// admin is authed plus the lldap_admin group. Authelia enforces the same rule
// on /admin; this is the second lock, in case the first is ever misconfigured.
func (s *Server) admin(next func(http.ResponseWriter, *http.Request, Identity)) http.Handler {
	return s.authed(func(w http.ResponseWriter, r *http.Request, id Identity) {
		if !id.Admin {
			http.Error(w, "admins only", http.StatusForbidden)
			return
		}
		next(w, r, id)
	})
}

// defaults are the admin's defaults over the configured ones.
func (s *Server) defaults() Settings {
	return s.store.Defaults().Or(s.cfg.Defaults)
}

// settings are the user's effective settings.
func (s *Server) settings(user string) Settings {
	return s.store.Settings(user).Or(s.defaults())
}

// --- the user's browser ---

// browser serves everything that is not the broker's own: it proxies to the
// caller's container, chosen by identity alone. A navigation to / with no
// running browser starts one and shows the waiting page.
func (s *Server) browser(w http.ResponseWriter, r *http.Request, id Identity) {
	if r.URL.Path == "/" && r.Method == http.MethodGet {
		c, err := s.docker.Inspect(r.Context(), id.User)
		if err != nil && !errors.Is(err, ErrNotFound) {
			slog.Error("inspect browser", "user", id.User, "err", err)
			http.Error(w, "kiosk non riesce a parlare con Docker", http.StatusBadGateway)
			return
		}
		if err != nil || !c.Running {
			s.start(id.User)
			s.waitPage(w)
			return
		}
	}
	upgrade := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	defer s.touch(id.User, upgrade)()
	s.proxy(id.User).ServeHTTP(w, r)
}

func (s *Server) proxy(user string) *httputil.ReverseProxy {
	target := s.target(user)
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			// What Caddy told us stays here: the browser container needs no
			// identity, and must never see the proxy secret.
			for _, h := range []string{"X-Kiosk-Proxy-Secret", "Remote-User", "Remote-Groups", "Remote-Email", "Remote-Name", "Cookie"} {
				pr.Out.Header.Del(h)
			}
			// Pages come back uncompressed so the overlay can be injected.
			if strings.Contains(pr.In.Header.Get("Accept"), "text/html") {
				pr.Out.Header.Del("Accept-Encoding")
			}
		},
		FlushInterval:  -1, // the stream is a WebSocket; never buffer
		ModifyResponse: injectOverlay,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Up but not listening yet: the waiting page polls until it is.
			if r.URL.Path == "/" && r.Method == http.MethodGet {
				s.waitPage(w)
				return
			}
			slog.Warn("proxy", "user", user, "path", r.URL.Path, "err", err)
			http.Error(w, "il tuo browser non è attivo: ricarica la pagina", http.StatusBadGateway)
		},
	}
}

// overlayTag is added to every HTML page of the browser UI.
const overlayTag = `<script src="/_kiosk/overlay.js" defer></script>`

func injectOverlay(resp *http.Response) error {
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	if i := bytes.LastIndex(body, []byte("</body>")); i >= 0 {
		body = slices.Concat(body[:i], []byte(overlayTag), body[i:])
	} else {
		body = append(body, overlayTag...)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

func (s *Server) waitPage(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	if err := pages.ExecuteTemplate(w, "wait.html", nil); err != nil {
		slog.Error("render wait page", "err", err)
	}
}

// start creates the user's browser in the background, once. Creation can take
// minutes (the first one may pull the image), so the request that asked for it
// gets the waiting page instead of hanging.
func (s *Server) start(user string) {
	s.mu.Lock()
	a := s.act(user)
	if a.starting {
		s.mu.Unlock()
		return
	}
	a.starting, a.startErr = true, ""
	s.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		err := s.create(ctx, user)
		s.mu.Lock()
		defer s.mu.Unlock()
		a := s.act(user)
		a.starting, a.lastSeen = false, s.now()
		if err != nil {
			slog.Error("create browser", "user", user, "err", err)
			a.startErr = "impossibile avviare il browser: " + err.Error()
		}
	}()
}

func (s *Server) create(ctx context.Context, user string) error {
	// A stopped one (crashed, or the host rebooted) is replaced, not
	// restarted: whatever session it held is stale.
	if err := s.docker.Remove(ctx, user); err != nil {
		return err
	}
	spec := s.cfg.Browser
	if img := s.store.Image(); img != "" {
		spec.Image = img
	}
	spec.MemoryMB = s.settings(user).MemoryMB
	err := s.docker.Create(ctx, spec, user)
	if errors.Is(err, ErrNotFound) {
		// The image is not on the host yet.
		slog.Info("pulling browser image", "image", spec.Image)
		if _, err := s.docker.Pull(ctx, spec.Image); err != nil {
			return err
		}
		err = s.docker.Create(ctx, spec, user)
	}
	if err != nil {
		return err
	}
	slog.Info("browser created", "user", user, "image", spec.Image)
	if err := s.store.MarkUsed(user, s.now()); err != nil {
		slog.Error("record last use", "user", user, "err", err) // only the admin list suffers
	}
	return nil
}

// ready tells the waiting page whether the browser UI answers yet.
func (s *Server) ready(w http.ResponseWriter, r *http.Request, id Identity) {
	s.mu.Lock()
	a := s.act(id.User)
	starting, startErr := a.starting, a.startErr
	a.lastSeen = s.now() // someone is waiting: that is watching
	s.mu.Unlock()

	resp := struct {
		Ready bool   `json:"ready"`
		Error string `json:"error,omitempty"`
	}{Error: startErr}
	if !starting && startErr == "" {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.target(id.User).String()+"/", nil)
		if res, err := http.DefaultClient.Do(req); err == nil {
			res.Body.Close()
			resp.Ready = res.StatusCode < 500
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}

// done destroys the caller's browser now. The downloads stay on the bind
// mount; the profile and cookies go with the container.
func (s *Server) done(w http.ResponseWriter, r *http.Request, id Identity) {
	if err := s.docker.Remove(r.Context(), id.User); err != nil {
		slog.Error("remove browser", "user", id.User, "err", err)
		http.Error(w, "impossibile distruggere il browser", http.StatusBadGateway)
		return
	}
	s.forget(id.User)
	slog.Info("browser destroyed", "user", id.User, "by", "user")
	w.WriteHeader(http.StatusNoContent)
}

type userSettings struct {
	StallMin int   `json:"stall_minutes"`
	Choices  []int `json:"choices"`
}

func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request, id Identity) {
	json.NewEncoder(w).Encode(userSettings{StallMin: s.settings(id.User).StallMin, Choices: StallChoices})
}

// setSettings lets a person pick their own stall window, from StallChoices
// only; the rest of their settings are the admin's.
func (s *Server) setSettings(w http.ResponseWriter, r *http.Request, id Identity) {
	n, err := strconv.Atoi(r.FormValue("stall_minutes"))
	if err != nil || !slices.Contains(StallChoices, n) {
		http.Error(w, "valore non ammesso", http.StatusBadRequest)
		return
	}
	set := s.store.Settings(id.User)
	set.StallMin = n
	if err := s.store.SetSettings(id.User, set); err != nil {
		slog.Error("save settings", "user", id.User, "err", err)
		http.Error(w, "impossibile salvare", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- admin ---

// browserRow is a live browser on the admin page.
type browserRow struct {
	User      string
	Created   time.Time
	Running   bool
	Viewers   int
	LastSeen  time.Time
	Downloads Downloads
}

// userRow is a person on the admin page, with their own overrides (0 = the
// default applies).
type userRow struct {
	User     string
	LastUsed time.Time
	Own      Settings
}

func (s *Server) adminPage(w http.ResponseWriter, r *http.Request, _ Identity) {
	var problems []string
	cs, err := s.docker.List(r.Context())
	if err != nil {
		slog.Error("admin: list", "err", err)
		problems = append(problems, "Docker non risponde: "+err.Error())
	}
	browsers := make([]browserRow, 0, len(cs))
	for _, c := range cs {
		b := browserRow{User: c.User, Created: c.Created, Running: c.Running,
			Downloads: scanDownloads(path.Join(s.cfg.Browser.StagingBase, c.User))}
		s.mu.Lock()
		if a, ok := s.activity[c.User]; ok {
			b.Viewers, b.LastSeen = a.viewers, a.lastSeen
		}
		s.mu.Unlock()
		browsers = append(browsers, b)
	}
	slices.SortFunc(browsers, func(a, b browserRow) int { return strings.Compare(a.User, b.User) })

	// Everybody who ever used kiosk, plus a browser kiosk did not record
	// (one created before state.json existed).
	names := s.store.Users()
	for _, b := range browsers {
		if !slices.Contains(names, b.User) {
			names = append(names, b.User)
		}
	}
	slices.Sort(names)
	list := make([]userRow, 0, len(names))
	for _, u := range names {
		list = append(list, userRow{User: u, LastUsed: s.store.LastUsed(u), Own: s.store.Settings(u)})
	}

	image := s.store.Image()
	if image == "" {
		image = s.cfg.Browser.Image
	}
	data := map[string]any{
		"Browsers": browsers,
		"Users":    list,
		"Image":    image,
		"Defaults": s.defaults(),
		"Problems": problems,
		"Msg":      r.URL.Query().Get("msg"),
	}
	w.Header().Set("Cache-Control", "no-store")
	if err := pages.ExecuteTemplate(w, "admin.html", data); err != nil {
		slog.Error("render admin", "err", err)
	}
}

func back(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/admin?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// formUser reads a user name from a form, refusing anything that is not
// already a SafeName: it becomes a container name and a path.
func formUser(r *http.Request) (string, bool) {
	u := strings.TrimSpace(r.FormValue("user"))
	return u, u != "" && SafeName(u) == u
}

// formSettings reads the three fields; an empty one means "inherit".
func formSettings(r *http.Request) (Settings, bool) {
	var set Settings
	for k, p := range map[string]*int{"stall_minutes": &set.StallMin, "viewer_idle_minutes": &set.ViewerIdleMin, "memory_mb": &set.MemoryMB} {
		v := strings.TrimSpace(r.FormValue(k))
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return set, false
		}
		*p = n
	}
	return set, set.Valid()
}

const invalidSettings = "Valori non validi: minuti da 1 a 1440, memoria da 512 a 65536 MB."

func (s *Server) adminDestroy(w http.ResponseWriter, r *http.Request, id Identity) {
	user, ok := formUser(r)
	if !ok {
		http.Error(w, "utente non valido", http.StatusBadRequest)
		return
	}
	if err := s.docker.Remove(r.Context(), user); err != nil {
		back(w, r, "Errore: "+err.Error())
		return
	}
	s.forget(user)
	slog.Info("browser destroyed", "user", user, "by", id.User)
	back(w, r, "Browser di "+user+" distrutto.")
}

func (s *Server) adminDefaults(w http.ResponseWriter, r *http.Request, id Identity) {
	set, ok := formSettings(r)
	if !ok {
		back(w, r, invalidSettings)
		return
	}
	if err := s.store.SetDefaults(set); err != nil {
		back(w, r, "Errore: "+err.Error())
		return
	}
	slog.Info("defaults changed", "by", id.User, "settings", set)
	back(w, r, "Default salvati.")
}

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request, id Identity) {
	user, ok := formUser(r)
	if !ok {
		http.Error(w, "utente non valido", http.StatusBadRequest)
		return
	}
	set, ok := formSettings(r)
	if !ok {
		back(w, r, invalidSettings)
		return
	}
	if err := s.store.SetSettings(user, set); err != nil {
		back(w, r, "Errore: "+err.Error())
		return
	}
	slog.Info("settings changed", "user", user, "by", id.User, "settings", set)
	back(w, r, "Impostazioni di "+user+" salvate.")
}

// adminImage pulls a new browser image and pins it by digest. It runs inside
// the request: the admin waits a minute on a pull they asked for.
func (s *Server) adminImage(w http.ResponseWriter, r *http.Request, id Identity) {
	ref := strings.TrimSpace(r.FormValue("ref"))
	if ref == "" || strings.ContainsAny(ref, " \t\n?#") {
		back(w, r, "Riferimento immagine non valido.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Minute)
	defer cancel()
	pinned, err := s.docker.Pull(ctx, ref)
	if err != nil {
		back(w, r, "Errore: "+err.Error())
		return
	}
	if err := s.store.SetImage(pinned); err != nil {
		back(w, r, "Errore: "+err.Error())
		return
	}
	slog.Info("browser image changed", "image", pinned, "by", id.User)
	back(w, r, "Immagine aggiornata a "+pinned+". Vale per i prossimi browser creati.")
}
