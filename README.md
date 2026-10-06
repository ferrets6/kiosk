# kiosk

Un browser usa-e-getta **sul NAS**, uno per persona, guidato dal proprio
browser. Serve a scaricare file grandi (es. Google Takeout) direttamente sul
NAS, a gigabit e anche a PC spento.

Il broker (Go, binario singolo, immagine distroless):
- identifica l'utente **solo** da `Remote-User`, creduto solo dalla rete del
  proxy e con l'header segreto `X-Kiosk-Proxy-Secret`;
- crea on demand il suo Chromium (`kiosk-browser-<utente>`) e gli inoltra
  tutto il traffico;
- lo distrugge quando nessuno lo guarda **e** nessun download si muove, o con
  "Fatto, distruggi". I download restano, profilo e cookie no;
- espone `/admin` (gruppo `lldap_admin`): browser accesi, default e override
  per utente, immagine del browser.

Parla con Docker solo tramite un `docker-socket-proxy`
(`CONTAINERS=1 IMAGES=1 POST=1`). Il design è in [`docs/design.md`](docs/design.md).
Il deploy (compose, Caddy, Authelia) sta nel repo `homelab`, in `services/kiosk/`.

## Configurazione

Le variabili sono in [`.env.example`](.env.example). Sono obbligatorie:
- `KIOSK_TRUSTED_PROXY`
- `KIOSK_PROXY_SECRET`
- `KIOSK_BROWSER_IMAGE` (per digest)
- `KIOSK_STAGING_BASE`: percorso sull'host, da montare nel broker allo stesso
  percorso.

## Sviluppo

```bash
make check   # vet, test -race, gofmt
make image
```

Un tag `v*` pubblica `ghcr.io/ferrets6/kiosk`.

## Licenza

MIT
