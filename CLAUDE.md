# kiosk

Broker that gives each person their own throwaway Chromium on the home NAS
(`lscr.io/linuxserver/chromium`, Selkies on :3000), routes them to it by SSO
identity, and destroys it when idle. First use: downloading Google Takeout
exports straight to the NAS (into zlatan's staging, which imports them).

Consumed by the `homelab` repo (`github.com/marcodellemarche/homelab`,
`services/kiosk/`): that repo holds only the compose, the Caddy block, the
Authelia rule and the Homepage card. The image is published here on a `v*` tag
to `ghcr.io/ferrets6/kiosk`. Design and rationale: `docs/design.md`.

## Binding decisions (don't relitigate without asking)

- Identity comes **only** from `Remote-User`, trusted only from
  `KIOSK_TRUSTED_PROXY` **and** with `X-Kiosk-Proxy-Secret`. Never from URL,
  cookie or form. Non-SafeName users are refused, not mangled (collisions
  would share a browser). `SafeName` must stay identical to zlatan's.
- One container per user, `kiosk-browser-<user>`, label `kiosk.user`. No
  global cap. The admin can see and destroy browsers, never enter one.
- Docker only through `tecnativa/docker-socket-proxy` (`CONTAINERS IMAGES
  POST`), on an internal network. The proxy can't see request bodies, so the
  container spec is **fixed in `docker.go`**: nothing from the request goes
  into it. Never add privileged, extra binds, or host networking.
- `/config` is not mounted (ephemeral profile). The image declares it as a
  VOLUME: removal must keep `v=1` or cookies survive on disk.
- The broker proxies everything (WebSocket included) and owns only
  `/_kiosk/` and `/admin`. It strips the secret, `Remote-*` and `Cookie`
  before forwarding.
- Reaper destroys (not stops) only when **both**: no viewer for the idle
  window AND no download moving (`*.crdownload`/`*.part`, top-level mtime)
  for the stall window. Defaults 15m / 10m / 2048 MB, editable in `/admin`
  (defaults and per user); users pick their own stall from `StallChoices`.
  Everything is capped at 24h.
- `/admin`: Authelia rule (`lldap_admin`) **plus** a `Remote-Groups` check in
  the broker. No LLDAP reading: it lists people who already used kiosk.
- Images pinned by digest; an admin-pulled tag is pinned via `RepoDigests`.

## Conventions

Go stdlib only, single package `internal/broker`, distroless nonroot (uid
65532 = zlatan's, so download dirs belong to zlatan). Comments in English,
SPDX `MIT` header, docs in Italian, dates `YYYY-MM-DD`. Table-driven tests;
CI runs `gofmt`, `go vet`, `go test -race`. Secrets only in `.env`. Low RAM
matters (24/7 power budget).
