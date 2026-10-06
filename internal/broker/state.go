// SPDX-License-Identifier: MIT

package broker

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// maxWindow caps every "keep it alive" setting. A browser logged into somebody's
// whole Google account should not outlive a day of nobody looking at it.
const maxWindow = 24 * time.Hour

// StallChoices are what a person may pick for their own browser, in minutes.
// The admin may set any value up to maxWindow.
var StallChoices = []int{10, 60, 240, 1440}

// Settings are the per-user knobs. Zero means "use the default".
type Settings struct {
	// StallMin: destroy only after no download has moved for this long.
	StallMin int `json:"stall_minutes,omitempty"`
	// ViewerIdleMin: ...and nobody has looked at it for this long.
	ViewerIdleMin int `json:"viewer_idle_minutes,omitempty"`
	// MemoryMB caps the browser's RAM. Applies from the next container.
	MemoryMB int `json:"memory_mb,omitempty"`
}

// Valid reports whether every non-zero field is within bounds.
func (s Settings) Valid() bool {
	max := int(maxWindow / time.Minute)
	return s.StallMin >= 0 && s.StallMin <= max &&
		s.ViewerIdleMin >= 0 && s.ViewerIdleMin <= max &&
		(s.MemoryMB == 0 || s.MemoryMB >= 512 && s.MemoryMB <= 65536)
}

// Or fills the zero fields from def.
func (s Settings) Or(def Settings) Settings {
	if s.StallMin == 0 {
		s.StallMin = def.StallMin
	}
	if s.ViewerIdleMin == 0 {
		s.ViewerIdleMin = def.ViewerIdleMin
	}
	if s.MemoryMB == 0 {
		s.MemoryMB = def.MemoryMB
	}
	return s
}

type stateFile struct {
	Image    string              `json:"image,omitempty"`
	Defaults Settings            `json:"defaults,omitempty"`
	Users    map[string]Settings `json:"users,omitempty"`
	// LastUsed is when each person last got a new browser: the admin page
	// lists everybody who ever used kiosk, not only who has one right now.
	LastUsed map[string]time.Time `json:"last_used,omitempty"`
}

// Store is state.json: the image the admin pinned and the per-user settings.
// ponytail: one small JSON file rewritten whole; a database when there are
// more than a handful of users.
type Store struct {
	mu   sync.Mutex
	path string
	s    stateFile
}

// OpenStore loads dir/state.json; a missing file is an empty state.
func OpenStore(dir string) (*Store, error) {
	st := &Store{path: filepath.Join(dir, "state.json")}
	b, err := os.ReadFile(st.path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	return st, json.Unmarshal(b, &st.s)
}

// Image is the admin's pinned image, or "" for the configured default.
func (st *Store) Image() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s.Image
}

// SetImage pins a new browser image.
func (st *Store) SetImage(ref string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	old := st.s.Image
	st.s.Image = ref
	if err := st.save(); err != nil {
		st.s.Image = old
		return err
	}
	return nil
}

// Settings returns the user's own overrides (zero fields = default).
func (st *Store) Settings(user string) Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s.Users[user]
}

// SetSettings replaces the user's overrides.
func (st *Store) SetSettings(user string, s Settings) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	old, had := st.s.Users[user]
	if st.s.Users == nil {
		st.s.Users = map[string]Settings{}
	}
	if s == (Settings{}) {
		delete(st.s.Users, user)
	} else {
		st.s.Users[user] = s
	}
	if err := st.save(); err != nil {
		if had {
			st.s.Users[user] = old
		} else {
			delete(st.s.Users, user)
		}
		return err
	}
	return nil
}

// Defaults are the admin's defaults for everyone (zero fields = the
// configured KIOSK_* default).
func (st *Store) Defaults() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s.Defaults
}

// SetDefaults replaces the admin's defaults.
func (st *Store) SetDefaults(s Settings) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	old := st.s.Defaults
	st.s.Defaults = s
	if err := st.save(); err != nil {
		st.s.Defaults = old
		return err
	}
	return nil
}

// MarkUsed records that user got a browser at t.
func (st *Store) MarkUsed(user string, t time.Time) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.s.LastUsed == nil {
		st.s.LastUsed = map[string]time.Time{}
	}
	st.s.LastUsed[user] = t
	return st.save()
}

// LastUsed is when user last got a browser, zero if never.
func (st *Store) LastUsed(user string) time.Time {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s.LastUsed[user]
}

// Users lists everybody who used kiosk or has overrides, sorted.
func (st *Store) Users() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	users := make([]string, 0, len(st.s.LastUsed))
	for u := range st.s.LastUsed {
		users = append(users, u)
	}
	for u := range st.s.Users {
		if _, ok := st.s.LastUsed[u]; !ok {
			users = append(users, u)
		}
	}
	slices.Sort(users)
	return users
}

// save writes atomically: a crash mid-write leaves the previous file.
func (st *Store) save() error {
	b, err := json.MarshalIndent(st.s, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}
