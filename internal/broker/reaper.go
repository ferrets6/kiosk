// SPDX-License-Identifier: MIT

package broker

import (
	"context"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"
)

// activity is what the broker has seen of one user's browser. It lives in
// memory: after a broker restart every browser counts as just seen, which errs
// on the side of not destroying a download.
type activity struct {
	lastSeen time.Time
	viewers  int // open WebSockets right now
	starting bool
	startErr string
}

// touch records a request for user. It returns a func to call when a
// long-lived connection (a WebSocket) ends; while one is open the user counts
// as watching.
func (s *Server) touch(user string, long bool) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.act(user)
	a.lastSeen = s.now()
	if !long {
		return func() {}
	}
	a.viewers++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		a.viewers--
		a.lastSeen = s.now()
	}
}

// act returns the user's record, creating it as "just seen". Caller holds s.mu.
func (s *Server) act(user string) *activity {
	a, ok := s.activity[user]
	if !ok {
		a = &activity{lastSeen: s.now()}
		s.activity[user] = a
	}
	return a
}

// Downloads is the state of a user's download directory.
type Downloads struct {
	InProgress bool      // a .crdownload/.part is there
	LastWrite  time.Time // newest modification time, zero if empty
}

// scanDownloads looks at the top level only: that is where Chromium writes,
// and zlatan may unpack large trees below it that are none of our business.
func scanDownloads(dir string) Downloads {
	var d Downloads
	entries, err := os.ReadDir(dir)
	if err != nil {
		return d
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".crdownload") || strings.HasSuffix(e.Name(), ".part") {
			d.InProgress = true
		}
		if info, err := e.Info(); err == nil && info.ModTime().After(d.LastWrite) {
			d.LastWrite = info.ModTime()
		}
	}
	return d
}

// reapable reports whether a browser may be destroyed: nobody watching for
// the idle window AND no download moving for the stall window. Either alone
// is not enough: the first would kill a download left running with the tab
// closed, the second a person reading a page.
func reapable(now time.Time, a activity, d Downloads, set Settings) bool {
	if a.starting || a.viewers > 0 {
		return false
	}
	if now.Sub(a.lastSeen) < time.Duration(set.ViewerIdleMin)*time.Minute {
		return false
	}
	if d.InProgress {
		return false
	}
	return now.Sub(d.LastWrite) >= time.Duration(set.StallMin)*time.Minute
}

// Reap runs until ctx is done, checking every interval.
func (s *Server) Reap(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reapOnce(ctx)
		}
	}
}

func (s *Server) reapOnce(ctx context.Context) {
	cs, err := s.docker.List(ctx)
	if err != nil {
		slog.Error("reaper: list browsers", "err", err)
		return
	}
	for _, c := range cs {
		if c.User == "" || SafeName(c.User) != c.User {
			continue // labelled by hand, not by us; leave it alone
		}
		s.mu.Lock()
		a := *s.act(c.User)
		s.mu.Unlock()
		why := "idle"
		if !c.Running {
			// A stopped browser is never coming back (restart "no"); its
			// layer still holds a profile, so it goes too.
			if a.starting {
				continue
			}
			why = "stopped"
		} else if !reapable(s.now(), a, scanDownloads(path.Join(s.cfg.Browser.StagingBase, c.User)), s.settings(c.User)) {
			continue
		}
		if err := s.docker.Remove(ctx, c.User); err != nil {
			slog.Error("reaper: remove", "user", c.User, "err", err)
			continue
		}
		s.forget(c.User)
		slog.Info("browser destroyed", "user", c.User, "by", "reaper", "why", why)
	}
}

// forget drops the user's activity, so a new browser starts with a clean slate.
func (s *Server) forget(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.activity[user]; ok && !a.starting {
		delete(s.activity, user)
	}
}
