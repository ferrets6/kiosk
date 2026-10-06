# kiosk — design

## Il problema

I link di Google Takeout non sono un'API: per scaricarli serve la **sessione
Google di un browser**. Scaricarli dal PC significa PC acceso, rete del PC
(spesso non gigabit) e poi un secondo trasferimento verso il NAS. kiosk mette
il browser **sul NAS**: il file nasce dove deve stare, a gigabit, e il download
continua anche a tab chiuso.

È un servizio generico ("apri un browser sul NAS"): Takeout è solo il primo
uso. Per Takeout i download atterrano nella staging di zlatan, che li importa.

## I pezzi

```
browser utente ──TLS──▶ Caddy ──forward_auth──▶ Authelia
                          │  + X-Kiosk-Proxy-Secret
                          ▼
                       kiosk (broker) ──HTTP──▶ docker-socket-proxy ──▶ docker.sock
                          │                       (rete internal)
                          ▼ (rete edge, per nome)
                 kiosk-browser-<utente>:3000  (Chromium + Selkies)
                          │
                          ▼ bind
                 <STAGING_BASE>/<utente>  → /config/Downloads
```

- **Broker**: l'unico pezzo privilegiato, quindi piccolo e leggibile (un
  package, nessuna dipendenza esterna). Distroless, uid 65532.
- **Browser**: uno per persona, effimero, creato e distrutto dal broker.
- **socket-proxy**: unico accesso a Docker, su una rete `internal` che solo il
  broker raggiunge.

## Identità e isolamento

L'utente viene **solo** da `Remote-User`, e solo se la richiesta arriva dal
CIDR fidato **e** porta l'header segreto (confronto a tempo costante). Mai da
URL, cookie o form: nessuno può chiedere il browser di un altro.

L'SSO dice chi sei, **non** isola le sessioni: lo fa il broker. Il container è
`kiosk-browser-<utente>` e il routing è funzione della sola identità, quindi A
non può finire nella sessione di B.

Un nome utente che non è già "sicuro" (`[A-Za-z0-9_-]`) viene **rifiutato**,
non trasformato: altrimenti `a.b` e `a_b` condividerebbero lo stesso browser.
La trasformazione `SafeName` è la stessa di zlatan, così le cartelle
coincidono.

Verso il browser il broker **toglie** l'header segreto, gli header `Remote-*`
e i cookie: il container non ha bisogno di sapere chi sei e non deve vedere
il segreto né la sessione Authelia.

`/admin` è protetto due volte: regola Authelia (`^/admin` solo per
`lldap_admin`) e controllo di `Remote-Groups` nel broker. Ogni POST
cross-site è rifiutato (`Sec-Fetch-Site`).

## Docker senza socket nudo

Il socket Docker in scrittura equivale a root sull'host. Il broker parla con
`tecnativa/docker-socket-proxy` configurato con `CONTAINERS=1 IMAGES=1
POST=1`, tutto il resto a 0.

Il proxy filtra per **path e metodo**, non per il **contenuto** della
richiesta: chi controllasse il broker potrebbe comunque creare un container
privilegiato. Per questo lo spec del browser è **fisso nel codice**
(`docker.go`, `Create`): immagine dalla config o dall'admin, un solo bind
(la cartella download), rete, `shm` 1 GB, `seccomp:unconfined`, restart `no`,
limite di memoria. Dalla richiesta dell'utente non entra niente.

Il client Docker sono poche chiamate HTTP della libreria standard: l'SDK
ufficiale sarebbe la parte più grande del binario per cinque endpoint.

## Ciclo di vita

1. `GET /` senza browser acceso → il broker lo crea **in background** e
   risponde subito con la pagina d'attesa. La pagina interroga
   `/_kiosk/ready`, che prova `GET :3000/` del container. Un container
   fermo (crash, riavvio dell'host con restart `no`) viene **sostituito**, non
   riavviato: la sessione che teneva è vecchia. Se l'immagine manca, il broker
   la scarica e riprova.
2. Browser acceso → il broker **inoltra tutto** (`httputil.ReverseProxy`,
   WebSocket compresi, `FlushInterval -1`). Inoltrare invece di reindirizzare
   tiene tutto su un'origine sola, e permette di misurare l'attività.
3. Nelle pagine HTML il broker inietta `/_kiosk/overlay.js`: la linguetta
   "kiosk ▾", in uno shadow root, con la scelta dello stallo e "Fatto,
   distruggi".

Il broker si riserva solo `/_kiosk/` e `/admin`; tutto il resto è del browser.

## Reaper

Ogni minuto, per ogni container con label `kiosk.user`, il browser viene
**distrutto** (`DELETE ?force=1&v=1`) quando valgono **entrambe**:

- **nessun viewer** da `ViewerIdle` minuti: nessun WebSocket aperto e nessuna
  richiesta. Un WebSocket aperto conta come "sta guardando" per tutta la sua
  durata;
- **nessun download in movimento** da `Stall` minuti: nessun `*.crdownload` o
  `*.part` e nessun file modificato nella cartella download (solo il primo
  livello: Chromium scrive lì, e sotto zlatan può scompattare alberi grandi).

Una sola delle due non basta: la prima ucciderebbe un download lasciato a tab
chiuso, la seconda una persona che legge una pagina.

L'attività sta in memoria. Dopo un riavvio del broker ogni browser conta come
"appena visto": nel dubbio non si distrugge un download.

`v=1` conta: l'immagine linuxserver dichiara `/config` come `VOLUME`, quindi
profilo e cookie stanno in un volume anonimo e non nel layer. Senza `v=1`
sopravvivrebbero al container. È verificato sul NAS.

## Impostazioni

| | Default | Chi la cambia |
|---|---|---|
| Stallo download | 10 min | admin (default e per utente), utente (10 min / 1 h / 4 h / 24 h) |
| Viewer inattivo | 15 min | admin (default e per utente) |
| Memoria | 2048 MB | admin (default e per utente), dal prossimo browser |

I default iniziali vengono da `KIOSK_*`; quelli dell'admin e gli override
stanno in `state.json` (`KIOSK_DATA_DIR`), insieme all'immagine scelta e
all'ultimo uso di ogni persona. Tutto ha un tetto di 24 h: è un browser loggato
nel Google di qualcuno.

L'immagine del browser si cambia da `/admin`: si indica un tag, il broker lo
scarica e lo **fissa al digest** (`RepoDigests`), così anche un tag mobile
diventa un riferimento immutabile. Vale per i browser creati dopo.

## Cosa non fa (di proposito)

- **Nessun tetto globale** ai browser: uno per persona basta. Ogni browser ha
  comunque un limite di memoria.
- **Nessuna lettura di LLDAP**: `/admin` elenca chi ha già usato kiosk.
  Leggere la directory vorrebbe un account e un segreto in più per una lista.
- **Nessun accesso dell'admin al browser altrui**: l'admin vede e distrugge,
  non entra. Entrare romperebbe l'isolamento, che è la garanzia principale.
- **Nessuna pulizia delle immagini vecchie** dopo un aggiornamento: si fa a
  mano (`docker image prune`).
