// SPDX-License-Identifier: MIT

package broker

import (
	"errors"
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"
	"time"
)

// Config is the whole of kiosk's configuration, read from KIOSK_* variables.
type Config struct {
	Addr         string
	TrustedProxy []*net.IPNet
	ProxySecret  string
	DockerHost   string
	Browser      BrowserSpec
	// DataDir holds state.json: the image the admin pinned and the per-user
	// overrides. Losing it only loses those choices, never a session.
	DataDir  string
	Defaults Settings
}

// Load reads the configuration through getenv (os.Getenv in production) and
// reports every problem at once, so a broken .env is fixed in one pass.
func Load(getenv func(string) string) (Config, error) {
	get := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	var errs []error
	required := func(k string) string {
		v := get(k, "")
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", k))
		}
		return v
	}

	cfg := Config{
		Addr:        get("KIOSK_ADDR", ":8080"),
		ProxySecret: required("KIOSK_PROXY_SECRET"),
		DockerHost:  strings.TrimRight(get("KIOSK_DOCKER_HOST", "http://kiosk-socket-proxy:2375"), "/"),
		Browser: BrowserSpec{
			Image:       required("KIOSK_BROWSER_IMAGE"),
			Network:     get("KIOSK_NETWORK", "edge"),
			StagingBase: required("KIOSK_STAGING_BASE"),
			StartURL:    get("KIOSK_START_URL", "https://takeout.google.com/"),
			TZ:          get("TZ", "Europe/Rome"),
		},
		DataDir: get("KIOSK_DATA_DIR", "/data"),
	}
	minutes := func(k, def string) int {
		d, err := time.ParseDuration(get(k, def))
		if err != nil || d < time.Minute || d > maxWindow {
			errs = append(errs, fmt.Errorf("%s must be a duration between 1m and 24h", k))
			return 0
		}
		return int(d / time.Minute)
	}
	cfg.Defaults.ViewerIdleMin = minutes("KIOSK_VIEWER_IDLE", "15m")
	cfg.Defaults.StallMin = minutes("KIOSK_DOWNLOAD_STALE", "10m")
	mem, err := strconv.Atoi(get("KIOSK_BROWSER_MEMORY_MB", "2048"))
	if err != nil || mem < 0 {
		errs = append(errs, errors.New("KIOSK_BROWSER_MEMORY_MB must be a number of MB (0 = no limit)"))
	}
	cfg.Defaults.MemoryMB = mem

	for _, part := range strings.Split(required("KIOSK_TRUSTED_PROXY"), ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			errs = append(errs, fmt.Errorf("KIOSK_TRUSTED_PROXY: %q is not a CIDR", part))
			continue
		}
		cfg.TrustedProxy = append(cfg.TrustedProxy, n)
	}
	if len(cfg.ProxySecret) > 0 && len(cfg.ProxySecret) < 32 {
		errs = append(errs, errors.New("KIOSK_PROXY_SECRET is too short (use `openssl rand -hex 32`)"))
	}
	// The convention is "never latest": a browser that is logged into
	// somebody's Google must be a known build.
	if cfg.Browser.Image != "" && !strings.Contains(cfg.Browser.Image, "@sha256:") {
		errs = append(errs, errors.New("KIOSK_BROWSER_IMAGE must be pinned by digest (image@sha256:...)"))
	}
	// It is a host path handed to Docker as a bind source; a relative one
	// would be resolved against the daemon's directory, not ours.
	if cfg.Browser.StagingBase != "" && !path.IsAbs(cfg.Browser.StagingBase) {
		errs = append(errs, errors.New("KIOSK_STAGING_BASE must be an absolute path"))
	}
	return cfg, errors.Join(errs...)
}
