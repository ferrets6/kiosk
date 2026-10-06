// SPDX-License-Identifier: MIT

// Package broker hands each person their own throwaway browser on the NAS and
// routes them to it by identity, never by anything they could choose.
package broker

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"slices"
	"strings"
)

// AdminGroup is the LLDAP group that may open /admin. It is the built-in
// directory-admin group, the same one Authelia gates every management tool on.
const AdminGroup = "lldap_admin"

// Identity is who is making a request, as the forward-auth proxy vouched for.
type Identity struct {
	User  string
	Admin bool
}

// Gate believes the forward-auth headers only when the request arrives from
// the trusted network AND carries the secret only Caddy knows. Either alone is
// not enough: any container on the edge network passes the first, and the
// second is what stops it skipping Caddy and the SSO.
type Gate struct {
	trusted []*net.IPNet
	secret  [32]byte
}

// NewGate builds a gate. Both settings are required by config validation; an
// empty secret here would still match nothing but an empty header, so it is
// refused there rather than tolerated here.
func NewGate(trusted []*net.IPNet, secret string) *Gate {
	return &Gate{trusted: trusted, secret: sha256.Sum256([]byte(secret))}
}

// Identify resolves the caller, or reports false. The user comes only from
// Remote-User: never from the URL, a cookie or a form, so a person cannot ask
// for somebody else's browser.
//
// A user whose name is not already a SafeName is refused rather than mangled:
// "a.b" and "a_b" would otherwise share one container, and one browser between
// two people is exactly what this service exists to prevent.
func (g *Gate) Identify(r *http.Request) (Identity, bool) {
	presented := sha256.Sum256([]byte(r.Header.Get("X-Kiosk-Proxy-Secret")))
	if subtle.ConstantTimeCompare(g.secret[:], presented[:]) != 1 || !g.fromTrusted(r) {
		return Identity{}, false
	}
	user := strings.TrimSpace(r.Header.Get("Remote-User"))
	if user == "" || SafeName(user) != user {
		return Identity{}, false
	}
	groups := strings.Split(r.Header.Get("Remote-Groups"), ",")
	for i := range groups {
		groups[i] = strings.TrimSpace(groups[i])
	}
	return Identity{User: user, Admin: slices.Contains(groups, AdminGroup)}, true
}

func (g *Gate) fromTrusted(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range g.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// SafeName turns a user name into a path element and a container-name suffix.
// It matches zlatan's sanitizer for every name Identify accepts (zlatan maps an
// empty result to "unknown"; kiosk refuses such a name instead): kiosk writes the downloads into
// the directory zlatan imports from, and two sanitizers that drift apart would
// silently stop finding each other's files.
func SafeName(in string) string {
	var b strings.Builder
	for _, r := range in {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
}
