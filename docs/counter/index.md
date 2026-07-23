# A realtime counter: Janus + Rip Server, end to end

A guided tour of Janus v1.2.0 in one page and one `app.rip`. You will build
a normal web app served over HTTPS that also has WSS support, where the
browser's secure WebSocket frames are proxied **by Janus as plain HTTP** to
the Rip server — the Janus↔Rip WS/HTTP bridge is the star. One page, one
`app.rip`, no extra infrastructure; the richness is small UI panels that
make each capability visible.

The page you will build shows a shared tally (+/− buttons), a live viewer
count, a WS round-trip badge, a "server time (cached 1s)" button, an admin
kick button, and a collapsible plumbing panel. Along the way you exercise
all four Janus capabilities — **ping** (the edge answers `/ping` itself,
proving site admission before any tenant exists), **control** (`/1.0`
registration, snapshot, counters), **cache** (the 1s micro-cache blip),
**hub** (every realtime act) — and Rip Server's strengths as the tenant:
the Sinatra-style DSL (the whole tenant is a handful of routes that read
naturally), watch-mode hot reload (the finale), the manager's
registration/heartbeat lifecycle (visible in `/1.0/apps`), and worker
disposability against socket permanence.

Everything below is exact and verified against the shipped contracts: the
hub contract ([`../20260720-162350-hub-design.md`](../20260720-162350-hub-design.md)),
the micro-cache contract
([`../20260720-033201-capability-microcache.md`](../20260720-033201-capability-microcache.md)),
the pool protocol
([`../20260719-002000-pool-protocol.md`](../20260719-002000-pool-protocol.md)),
and the `@rip-lang/server` sources. Where the demo makes a judgment call,
**Design notes** at the end explains it.

Open two browser windows side by side and this is the show, as acts:

| Act | Action | Both windows show | What it proves |
| --- | --- | --- | --- |
| 1 — realtime tally | Browser 1 clicks **+** ×4, **−** ×1 | 1, 2, 3, 4, then 3 | WSS frame → text bridge (HTTP) → response directives → WSS fan-out |
| 2 — presence | Open a third window; close it | viewers 2 → 3 → 2 | open/close bridge lifecycle + the membership snapshot |
| 3 — liveness badge | Watch the corner badge | `~1 ms` RTT, refreshed every 5s | `?`/`!` answered at the edge; worker never touched |
| 4 — cache blip | Click "server time" fast in both windows | identical timestamp, `X-Janus-Cache: HIT`, `Age` | micro-cache riding the same site, zero tenant code |
| 5 — hot reload (finale) | Edit `STEP = 1` → `2`, save; browser 2 clicks **+** ×2 | 3 → 5 → 7; sockets never drop | doorbell reload invisible to hub clients |
| 6 — kick + reconnect (encore) | Click "admin: disconnect everyone" | "kicked: admin kick", then auto-reconnect, tally restored | trusted `*` kick, close bridges, reconnection as recovery, late-joiner state sync |

## 1. The layout

Two checkouts, side by side. This tutorial uses `~/src` as the running
example — substitute your own directory throughout:

```text
~/src/janus   # this repo — the edge (github.com/shreeve/janus)
~/src/rip     # the Rip language + @rip-lang/server (github.com/shreeve/rip)
```

The demo's tenant runs on `@rip-lang/server` from
[github.com/shreeve/rip](https://github.com/shreeve/rip) — clone it next
to janus. The demo files `app.rip` and `Caddyfile.demo` sit next to this
page (`docs/counter/` in the janus repo) — download or copy them; the
tutorial shows their full contents below, and the shipped files are
byte-identical to the listings.

## 2. Architecture

```text
  Browser 1            Browser 2            Browser 3 (act 2)
     │  HTTPS: GET /, /now, /plumbing   +   WSS: /hub
     └───────────────────┬───────────────────┘
                         ▼  TLS :443 (certs/ripdev.io.*, *.ripdev.io → 127.0.0.1)
        ┌──────────────────────────────────────────────────┐
        │  Janus  (./bin/caddy, one process)               │
        │   ├── data plane   GET / /now /plumbing → worker │
        │   ├── cache        1s micro-cache on this site   │
        │   ├── hub          WS terminates HERE; ?/! pong, │
        │   │                membership, fan-out, * kick   │
        │   └── control /1.0 http://127.0.0.1:7600         │
        └────────┬───────────────────▲─────────────────────┘
    unix socket  │ POST /rt/bridge   │ register + heartbeat (5s),
    (data plane) │ (open/text/close) │ upstreams PUTs, doorbell;
                 ▼                   │ tenant reads /1.0/apps/{id}/hub
        ┌──────────────────────────────────────────────────┐   and /1.0/hub
        │  rip manager (bun … server.rip, watch mode)      │
        │   └── worker ×1 (app.rip) — owns tally + STEP    │
        └──────────────────────────────────────────────────┘
```

Three named flows you will see over and over:

1. **Page load (HTTPS → data plane → worker).** `GET https://demo.ripdev.io/`
   resolves the host in the registry and proxies to the worker's unix
   socket. `/now` and `/plumbing` ride the same path — `/now` through the
   micro-cache, `/plumbing` declaring `no-store` so it never caches.
2. **Interaction (WSS frame → text bridge HTTP POST → response directives →
   WSS fan-out).** A click sends `{"increment": {}}` on the socket. Janus
   validates and executes it at the edge, then POSTs the frame verbatim to
   the tenant's registered `bridge_path` as a plain HTTP request over the
   same data plane. The worker's answer body carries directives, which
   Janus executes and fans out over WSS. This bridge POST is the WSS→HTTP
   proxying moment the demo exists to show.
3. **Reload (doorbell; sockets survive).** A save settles (~150ms), passes
   the content-hash gate, and cuts admission with one doorbell PUT. The
   next demand — here, the next click's bridge POST — rings, the manager
   boots a fresh pool, publishes sockets, and the held bridge completes
   against the fresh worker. Hub sockets, channels, and fan-out ride above
   the worker plane and never notice.

The control-plane publish endpoint
(`POST http://127.0.0.1:7600/1.0/apps/{id}/hub/publish`, `@` required per
object) is **not** in this demo's main loop; a server-initiated broadcast
(cron job, admin script) would use it. On that plane there is no
originating connection, so bare and `!`-suffixed event spellings deliver
identically — unlike the bridge-response plane used throughout below.

## 3. Prerequisites and launch

You need Go (current stable), `xcaddy`, `bun`, and the two checkouts from
section 1. DNS for `*.ripdev.io` resolves to `127.0.0.1` and the trusted
wildcard cert is committed in the janus repo's `certs/` — HTTPS on
localhost works out of the box.

Every command below spells the checkouts as `~/src/janus` and `~/src/rip`;
substitute your own paths if they live elsewhere.

**Build the Janus caddy binary** (from `~/src/janus`):

```bash
cd ~/src/janus
export PATH="$(go env GOPATH)/bin:$PATH"
mkdir -p bin
xcaddy build --with github.com/shreeve/janus=. --output ./bin/caddy
# (without a checkout, pin instead: xcaddy build --with github.com/shreeve/janus@v1.2.0 --output ./caddy)
```

**The demo Caddyfile.** It ships next to this page as
`docs/counter/Caddyfile.demo`. One site; hub on with `origin same` (the
page and the socket share `demo.ripdev.io`, so the default posture
passes); cache on with its 1s default TTL plus the `debug` knob so act 4's
`X-Janus-Cache` verdict is visible; control on loopback `:7600`.

```caddyfile
{
	auto_https disable_redirects
	skip_install_trust

	janus {
		ping # GET /ping → pong at the edge; default is off, and the rip app has no /ping route to fall through to
		control local # http://127.0.0.1:7600 — the manager registers here; the tenant reads snapshots/counters
		cache {
			debug # X-Janus-Cache: HIT|MISS|COALESCED|BYPASS on every response; ttl stays the 1s default
		}
		hub # path /hub, origin same, max_conns 4096 — the defaults
	}
}

demo.ripdev.io {
	tls certs/ripdev.io.crt certs/ripdev.io.key
	janus
}
```

With cache on, `GET /` is an anonymous GET and may serve from memory for
1s — the inlined tally can be one second stale on a fresh window. That is
harmless here by design: act 6's open-bridge greeting pushes the current
tally over the socket the moment the window enrolls.

**Launch Janus.** Run caddy from the janus checkout root — the cert paths
in `Caddyfile.demo` are relative to where caddy runs (binding :443 may
need sudo on some systems):

```bash
cd ~/src/janus
./bin/caddy run --config docs/counter/Caddyfile.demo
```

**Create and launch the rip app.** The app directory needs
`node_modules/@rip-lang/server` resolving to the rip checkout, and the
manager is launched with bun's `--preload` loader plus `--name`, `--host`,
`--workers`, and `--control`:

```bash
mkdir -p ~/counter-demo && cd ~/counter-demo
cp ~/src/janus/docs/counter/app.rip .   # the sibling file, listed in full in section 4
mkdir -p node_modules/@rip-lang
ln -sfn ~/src/rip/packages/server node_modules/@rip-lang/server

bun --preload=$HOME/src/rip/src/loader.js \
  $HOME/src/rip/packages/server/server.rip \
  --name demo --host demo.ripdev.io --workers 1 \
  --control http://127.0.0.1:7600
```

Watch mode is on by default (`RIP_ENV` unset): the manager registers,
heartbeats every 5s, cuts admission with the doorbell, and boots on the
first demand. Startup prints
`rip-server: demo registered (demo-xxxxxx) — 1 worker(s), watch on`.
`--workers 1` is deliberate: the tally and `STEP` live in one worker's
memory for demo clarity. The production path is to externalize that state
(a database, a KV store), after which `--workers N` and the data plane's
least-conn selection shine unchanged.

**Register the bridge path.** The manager registers only `name` and
`hosts` — it does not know about `bridge_path` (verified in the rip
checkout's `packages/server/manager.rip`, `registerApp`). Without a
registered `bridge_path`, every hub handshake answers 503 by contract.
Wire it once, by hand, against the app id the manager just minted:

```bash
APP_ID=$(curl -s http://127.0.0.1:7600/1.0/apps | jq -r '.[] | select(.name=="demo") | .id')
curl -s -X PATCH -H 'Content-Type: application/json' \
  --data '{"bridge_path":"/rt/bridge"}' \
  "http://127.0.0.1:7600/1.0/apps/$APP_ID"
```

**Verify:**

```bash
curl -s https://demo.ripdev.io/ping           # → pong, answered at the edge: site admission proven, worker never touched — the primordial capability
curl -s http://127.0.0.1:7600/1.0/apps        # the demo app: id, hosts, upstreams (the manager's lifecycle, visible), "bridge_path":"/rt/bridge"
curl -s https://demo.ripdev.io/               # the HTML page (first hit may ride the initial boot)
curl -s http://127.0.0.1:7600/1.0/hub         # hub counters, all zeros before the first socket
curl -s http://127.0.0.1:7600/1.0/cache       # cache counters
```

## 4. The rip app

Complete `app.rip` — the whole tenant, shipped next to this page.
Sinatra-style DSL: every act below is one plainly-readable route. The
tally is persisted under `tmp/` because a hot reload scraps the worker
pool and with it all module state; `tmp/` is in the watcher's ignore list
(`IGNORED_DIRS` in `manager.rip`) and is skipped by the content-hash gate,
so writing it neither triggers nor is lost to reloads. Without this file
the finale would show 0 → 2 → 4 instead of 3 → 5 → 7.

The app id (needed for the snapshot URL and the plumbing panel) is not in
the worker's environment — the manager passes only `APP_ARTIFACT`,
`SOCKET_PATH`, `WORKER_ID`, `WORKER_CONCURRENCY`, `RIP_DRAIN_DEADLINE_MS`
(verified in `manager.rip`, `spawnWorker`). The clean mechanism: resolve
it lazily with `GET /1.0/apps`, match by name, cache it, and re-resolve on
a snapshot 404 (a Janus restart makes the manager re-register under a new
id).

```coffee
import { get, post, read, start } from '@rip-lang/server'
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs'

# The hot-reload act (act 5) edits this constant to 2 and saves.
STEP = 1

CHANNEL = '/stats/tally'
CONTROL = 'http://127.0.0.1:7600'

# Authoritative tally, persisted so it survives pool reloads. tmp/ is
# ignored by the manager's watcher, so writes here never cause a reload.
DIR  = "#{import.meta.dir}/tmp"
FILE = "#{DIR}/tally"
mkdirSync DIR, { recursive: true }
tally = 0
try tally = parseInt(readFileSync(FILE, 'utf8'), 10) or 0
save = -> writeFileSync FILE, String(tally)

# --- app id + membership snapshot (control plane) -----------------------------
# The worker env carries no app id; resolve by name and cache. A 404 on the
# snapshot means Janus restarted and the manager re-registered under a new
# id — re-resolve once.
appId = null

resolveAppId = ->
  res  = fetch! "#{CONTROL}/1.0/apps"
  apps = res.json!
  appId = apps.find((a) -> a.name is 'demo')?.id

snapshot = ->
  resolveAppId!() unless appId?
  res = fetch! "#{CONTROL}/1.0/apps/#{appId}/hub"
  if res.status is 404
    resolveAppId!()
    res = fetch! "#{CONTROL}/1.0/apps/#{appId}/hub"
  res.json!   # { conns, channels: {"/stats/tally": N}, connections: {opaque handles} }

members = ->
  snap = snapshot!()
  snap.channels[CHANNEL] or 0

# --- routes -------------------------------------------------------------------

get '/' -> PAGE   # PAGE is the HTML constant at the bottom of this file

# Act 4: the micro-cache blip. Anonymous GET, storable response; Janus
# caches it for 1s. Zero cache code here — the capability rides the site.
get '/now' -> { now: new Date().toISOString() }

# Plumbing panel: the tenant reading its own reflection off the control
# plane. no-store keeps this poll out of the micro-cache by contract.
get '/plumbing' ->
  @header 'Cache-Control', 'no-store'
  resolveAppId!() unless appId?
  res   = fetch! "#{CONTROL}/1.0/hub"
  stats = res.json!
  mine  = stats.apps[appId] or {}
  { app: appId, pid: process.pid, step: STEP,
    conns: mine.conns, frames_in: mine.frames_in,
    deliveries: mine.deliveries, publishes: mine.publishes }

# The hub bridge: Janus POSTs every socket event here with
# Sec-WebSocket-Frame: open | text | close. The route is TOLERANT: frames
# it does not recognize (the ?-pings of act 3, future events) fall through
# to the 204 at the bottom — observed, nothing to say.
post '/rt/bridge' ->
  kind = @req.header 'sec-websocket-frame'

  # open: the admission decision, AND enrollment + greeting. The list
  # executes in order on the new connection: join first, then deliver.
  # The snapshot cannot include this connection yet (it registers only
  # after this 2xx) — hence the +1.
  if kind is 'open'
    n = members!() + 1
    return [
      { '+': [CHANNEL] }
      { 'tally!': { value: tally } }
      { '@': [CHANNEL], 'viewers!': { count: n } }
    ]

  # close: fires after Janus's local cleanup, so the snapshot already
  # excludes the departed connection — the count is exact.
  if kind is 'close'
    return { '@': [CHANNEL], 'viewers!': { count: members!() } }

  return null unless kind is 'text'

  # Act 1: the counter. The client's frame arrives verbatim as the JSON
  # body; read() sees its top-level keys.
  if read('increment')? or read('decrement')?
    tally += STEP if read('increment')?
    tally -= STEP if read('decrement')?
    save()
    # Bridge-response plane: the ORIGINATING connection is the exclusion
    # context — the "!" suffix is REQUIRED. See the callout below.
    return { '@': [CHANNEL], 'tally!': { value: tally } }

  # Act 6: the kick. Trusted-plane "*": every resolved member — including
  # the clicker — receives {"*":"admin kick"} then close 1000.
  if read('kickall')?
    return { '@': [CHANNEL], '*': 'admin kick' }

  null   # ?-pings and anything else: observed, 204

# --- the page ------------------------------------------------------------------

PAGE = """
<!doctype html>
<html>
<head><meta charset="utf-8"><title>Janus tour</title></head>
<body>
  <h1 id="tally">#{tally}</h1>
  <button onclick="send('increment')">+</button>
  <button onclick="send('decrement')">-</button>
  <p id="viewers"></p>
  <p id="rtt" style="position:fixed;top:8px;right:8px"></p>
  <p><button onclick="now()">server time (cached 1s)</button> <span id="now"></span></p>
  <p><button onclick="send('kickall')">admin: disconnect everyone</button> <span id="kick"></span></p>
  <details><summary>plumbing</summary><pre id="plumbing"></pre></details>
  <script>
    var tallyEl = document.getElementById('tally');
    var viewersEl = document.getElementById('viewers');
    var rttEl = document.getElementById('rtt');
    var nowEl = document.getElementById('now');
    var kickEl = document.getElementById('kick');
    var plumbingEl = document.getElementById('plumbing');
    var ws, backoff = 500;

    function connect() {
      ws = new WebSocket('wss://' + location.host + '/hub');
      ws.onopen = function () { backoff = 500; kickEl.textContent = ''; };
      ws.onmessage = function (e) {
        var msg = JSON.parse(e.data);
        if (msg['!'] !== undefined) rttEl.textContent = (Date.now() - Number(msg['!'])) + ' ms';
        if (msg.tally) tallyEl.textContent = msg.tally.value;
        if (msg.viewers) viewersEl.textContent = msg.viewers.count + ' viewer(s) online';
        if (msg['*'] !== undefined) kickEl.textContent = 'kicked: ' + msg['*'];
      };
      ws.onclose = function () {           // kicked, or Janus restarted:
        setTimeout(connect, backoff);      // reconnect re-enrolls through a
        backoff = Math.min(backoff * 2, 5000); // fresh open bridge
      };
    }

    function send(name) {
      if (!ws || ws.readyState !== 1) return;
      var frame = {};
      frame[name] = {};
      ws.send(JSON.stringify(frame));
    }

    function now() {
      fetch('/now').then(function (r) {
        var v = r.headers.get('x-janus-cache'), age = r.headers.get('age');
        r.json().then(function (j) {
          nowEl.textContent = j.now + ' [' + v + (age !== null ? ', Age ' + age : '') + ']';
        });
      });
    }

    setInterval(function () {              // act 3: in-band liveness
      if (ws && ws.readyState === 1) ws.send(JSON.stringify({'?': String(Date.now())}));
    }, 5000);

    setInterval(function () {              // plumbing poll (HTTPS, no-store)
      fetch('/plumbing').then(function (r) { return r.json(); })
        .then(function (j) { plumbingEl.textContent = JSON.stringify(j, null, 2); });
    }, 2000);

    connect();
  </script>
</body>
</html>
"""

start()
```

Notes on why this is correct against the shipped contracts:

- The `open` bridge POST has an empty body and no `Content-Type`; the rip
  framework parses nothing and the handler returns the directive list,
  which ships as 200 JSON. List frames execute mutations first, in object
  order, then resolve each object's `@` against **post-mutation**
  membership — so the `+` join lands before the two deliveries resolve.
- In the open response, `tally!` carries no `@`: on the bridge-response
  plane an absent `@` defaults to the originating connection — the browser
  being admitted. That is the late-joiner state sync: enrollment and
  greeting in one answer. The `!` is required even here — a bare `tally`
  would exclude the originating connection and deliver to nobody.
- `viewers!` in the open response does carry `@`: it must reach every
  member, newcomer included (just joined, so the channel resolves to it
  too). The count is snapshot + 1 because the admission completes only
  after the tenant's 2xx (concurrent opens can transiently undercount;
  demo-scale, acceptable).
- On close, directives still execute with the dead connection as origin;
  it has already left every channel, so `/stats/tally` resolves to the
  survivors only. If the last window closes, the emptied channel is
  deleted and the `@` entry counts as an unknown target — legal, delivers
  to nobody, harmless.
- `read('increment')?` is a presence test: the parsed body for
  `{"increment":{}}` carries the key with value `{}`, and `read` returns
  it (or `null` when absent). No validator needed.
- The kick object resolves its recipients from `@` like any delivery, and
  kicks are not events — sender exclusion does not apply. Every member,
  including the clicker, receives `{"*":"admin kick"}` as its final
  application frame and then close 1000 (verified in `hub.go`, `execute`).
- Returning `null` gives 204 — the "observed, no directives" bridge
  answer. This is what makes the route tolerant: `?`-pings and unknown
  events are observed and ignored, never errored.

> **The `!` footgun (do not trip it).** Bridge-response directives execute
> with the bridged frame's originating connection as the sender-exclusion
> context — the browser that clicked (or, for open, the one being
> admitted). A bare `"tally"` would deliver to every `/stats/tally` member
> **except that connection**: the other window updates, yours doesn't —
> and the open-response greeting would vanish entirely. The `!` suffix
> (include-sender delivery) reaches the full set; recipients always see
> the key `tally` — the suffix is a wire directive, stripped on delivery.
> Contrast the publish plane, where no originating connection exists and
> both spellings deliver identically. (Hub contract, "The `!` suffix",
> rules 1–3.)

## 5. The acts, walked through exactly

**Act 1 — realtime tally.** Open two windows on
`https://demo.ripdev.io/` (flow 1); each opens
`wss://demo.ripdev.io/hub`. Per handshake Janus checks origin (`same` —
the page's Origin equals the request Host, passes), reserves a slot, mints
a 16-char connection id, and sends the **open bridge** while the handshake
holds:

```http
POST /rt/bridge HTTP/1.1
Host: demo.ripdev.io
Sec-WebSocket-Frame: open
Janus-Hub-Client: k7f2m9x0q4w1z8p3
Janus-Hub-App: demo-x7k2p9
(plus the frozen handshake-header snapshot: Origin, Cookie, User-Agent, …)
```

The worker answers 200 with the enrollment + greeting list; Janus enrolls
the connection, completes the upgrade (101), and processes the directives
before the inbound reader starts — greeting always precedes the first
click. A **+** click then sends one WSS text frame:

```json
{"increment": {}}
```

A legal client-plane event frame. It carries no `@`, so delivery targets
default to the sending connection, and a bare event name excludes the
sender — the frame **delivers to nobody, by design**. That is not a loss:
the contract states the empty sender-excluded delivery is legal and
counted as zero, and this frame's job is not delivery. It exists to be
**observed by the text bridge**, which Janus sends regardless of delivery
outcome. This is the WSS→HTTP proxying moment — the exact HTTP request the
rip route receives:

```http
POST /rt/bridge HTTP/1.1
Host: demo.ripdev.io
Sec-WebSocket-Frame: text
Janus-Hub-Client: k7f2m9x0q4w1z8p3
Janus-Hub-App: demo-x7k2p9
Content-Type: application/json
X-Forwarded-For: 127.0.0.1
(plus the same frozen handshake snapshot as at open)

{"increment":{}}
```

The body is the client's frame **verbatim** — the exact bytes from the
socket, never re-serialized. The worker computes `tally = 0 + 1`, persists
it, and answers 200 with
`{"@": ["/stats/tally"], "tally!": {"value": 1}}`. Janus resolves
`/stats/tally` to both connections, includes the sender (the `!`), strips
the suffix, and both browsers receive:

```json
{"tally":{"value":1}}
```

Four **+** and one **−** later, both windows show 3.

**Act 2 — presence.** Open a third window. Its open bridge fires; the
worker fetches `GET http://127.0.0.1:7600/1.0/apps/demo-x7k2p9/hub` —

```json
{"conns": 2, "channels": {"/stats/tally": 2}, "connections": {"c1": ["/stats/tally"], "c2": ["/stats/tally"]}}
```

— counts and opaque handles, never raw connection ids. It answers the
open with count 2 + 1 = 3; all three windows render "3 viewer(s) online",
and the newcomer also receives the `tally!` greeting (3, the current
value — not the possibly 1s-stale inlined number). Close the third
window: Janus cleans up locally (channel membership drops to 2), then
fires the **close bridge** (`Sec-WebSocket-Frame: close`, body
`{"code": 1001, "reason": "…"}`); the worker snapshots again — now exact,
post-cleanup — and fans out `viewers!` count 2. Open, text, close: the
full bridge lifecycle, plus the snapshot as the tenant's resync
instrument.

**Act 3 — liveness badge.** Every 5s each page sends `{"?": "<epoch-ms>"}`
(the `?` value must be a JSON string ≤ 128 bytes — hence `String(Date.now())`).
Janus answers `{"!": "<echoed>"}` **from the edge — the worker is never
touched**; that is the contract's point: browser JavaScript cannot send
protocol-level pings, so `?`/`!` is in-band liveness, and the echoed value
lets the page compute the round trip it renders. The frame is still
forwarded to the text bridge for observation like every client frame —
which is why the bridge route is written tolerant: it answers 204 to
anything that is not increment/decrement/kickall. Watch `frames_in` and
`bridge_sent` tick every 5s per window in the plumbing panel while the
worker's route does nothing but 204.

**Act 4 — the micro-cache blip.** Click "server time (cached 1s)" rapidly
in both windows. The route is one line server-side; the caching is
entirely Janus. The doorkeeper admits a key on its **second** sighting, so
the pattern within one second is: first click `X-Janus-Cache: MISS`,
second `MISS` (now stored), third and later `HIT` with an `Age` header —
and the clicks land in *either* window, because both share the cache key.
Identical timestamp across windows until the 1s TTL lapses, then a fresh
`MISS` refills. The page displays the verdict and `Age` next to the
timestamp. Meanwhile the plumbing poll shows `MISS` forever on its own
route: `/plumbing` declares `Cache-Control: no-store`, and no-store
responses are never stored, by contract.

## 6. Act 5 — the hot-reload finale

Edit one line in `~/counter-demo/app.rip` and save:

```coffee
STEP = 2
```

Mind which file you edit: the manager's watcher sees only the copy in
`~/counter-demo/` — the running app directory. Editing the repo's
`docs/counter/app.rip` (the shipped artifact this copy came from) changes
nothing until you copy it over.

The manager's watcher fires; the save settles (~150ms) and the changed
content hash triggers the reload epoch: one doorbell PUT replaces the
worker sockets, cutting admission. Nothing boots yet — watch mode is lazy;
the next demand rings.

Click **+** in browser 2. The frame executes at the edge as always, and
its text-bridge POST becomes that demand: it selects the doorbell, rings,
the manager compiles the app once and boots a fresh worker (which reads
`tmp/tally` → 3), publishes the fresh socket with a sockets PUT, and the
held bridge POST completes against the new worker. `3 + 2 = 5`, the
response directives fan out, both windows render 5. Another click: 7.
Browser 1 did nothing and watched it happen. The plumbing panel makes the
disposability visible: `pid` changed, `step` reads 2, `conns` never moved.

Why the sockets survive — the contract's own words. The reload table in
the hub design doc pins, for **Doorbell PUT (admission cut)** and the
dirty window alike: open sockets **Untouched**, membership **Untouched**;
and the doc states the consequence plainly:

> **An app reload is invisible to connected hub clients.** Sockets,
> channels, and fan-out ride above the worker plane; the only
> reload-visible artifact is delayed (or, past the ring caps, dropped)
> tenant *observation* of text frames, and a one-boot delay on new
> handshakes.

Connections terminate at Janus; workers are disposable by contract; hub
teardown keys off registration lifecycle (DELETE / TTL reap), never off
upstreams PUTs. The doorbell cut that scraps the pool cannot touch a
socket. Worker disposability and socket permanence, on one screen.

## 7. Act 6 — kick + reconnect, the encore

Click "admin: disconnect everyone" in either window. The button sends
`{"kickall": {}}` — like the counter frames, it delivers to nobody and
exists to be observed. The bridge route answers with the trusted-plane
kick:

```json
{"@": ["/stats/tally"], "*": "admin kick"}
```

Every `/stats/tally` member — the clicker included; kicks resolve targets
from `@` with no sender exclusion — receives `{"*":"admin kick"}` as its
final application frame, renders "kicked: admin kick", and is closed with
code 1000. Each close fires a close bridge; the worker fans out the
shrinking viewer count.

Then the recovery story runs by itself: each page's `onclose` reconnects
with small backoff. Each reconnect is a **full open bridge** — the tenant
re-authenticates and re-enrolls every returning connection, and the
greeting delivers the authoritative tally (from worker memory, backed by
`tmp/tally`). Within a couple of seconds both windows are back: enrolled,
viewer count restored, tally exactly where it was. Reconnection is the
hub's recovery mechanism for every disruption class — kick, slow-consumer
close (1013), and Janus restart alike — and the open response is where
late joiners resync.

## 8. Verification and troubleshooting

**Counters** — `curl -s http://127.0.0.1:7600/1.0/hub` (process totals +
per-app breakdown under `"apps"`, keyed by app id):

- `conns: 2`, `channels: 1` with two windows open.
- `frames_in` ticks per click and per 5s ping; `deliveries` +2 per counter
  click with two windows (the bridge-response fan-out; the click frame
  itself contributes zero — empty sender-excluded delivery, counted as
  zero) and +1 per pong-window per viewer update.
- `bridge_sent` per client frame; `bridge_failed` / `bridge_dropped`
  should stay 0 outside reload windows; `publishes` stays 0 (this demo
  never uses the publish plane).

**Membership snapshot** —
`curl -s http://127.0.0.1:7600/1.0/apps/$APP_ID/hub` shows
`"channels": {"/stats/tally": 2}` and opaque handles. **Cache counters** —
`curl -s http://127.0.0.1:7600/1.0/cache` moves during act 4.

**Common failures:**

- **WS handshake answers 503 + `Retry-After`** — no `bridge_path`
  registered (hub-enabled site, hub-unready tenant, loud by contract).
  Re-run the PATCH from section 3. Note: if Janus restarts, the manager
  re-registers under a **new app id** with only `name`/`hosts` — the
  `bridge_path` is gone until you PATCH the new id.
- **WS handshake answers 403** — origin policy. Served page and hub share
  `demo.ripdev.io` so `origin same` passes for browsers; a non-browser
  client (curl, wscat) sends no `Origin` header and fails `same` — that is
  the contract's posture, not a bug. Test WS by hand only via the page, or
  add an `origin any` site.
- **Your own window doesn't update but the other one does** — a bridge
  response used bare `"tally"` (or `"viewers"`) instead of the `!`
  spelling. That delivery excluded the originating connection. See the
  callout in section 4.
- **Sockets close 1008 after adding the ping timer** — the `?` value must
  be a JSON **string**; `{"?": 1721512345}` is malformed and Janus closes
  the connection with the positioned reason. Send `String(Date.now())`.
- **Bridge route breaks when acts are added** — a route that only expects
  increment/decrement will also receive `?`-pings and `kickall` and every
  future frame (all client frames bridge). It must fall through to 204,
  never 4xx/5xx: a non-2xx text answer counts `bridge_failed` (client
  unaffected), and a non-JSON error page would count `bridge_garbage`.
  Write the bridge route tolerant, as in section 4.
- **Snapshot answers 404** — stale app id (Janus restarted; the manager
  re-registered and minted a new one). The `snapshot` helper re-resolves
  by name on 404; if presence stops updating, check
  `GET /1.0/apps` and re-PATCH `bridge_path` under the new id.
- **Kick reason doesn't render** — the `{"*":"admin kick"}` application
  frame arrives **before** the close; handle it in `onmessage` (the
  `msg['*']` branch), not in `onclose` — the WebSocket close code is 1000
  and the wire reason is not the place the payload travels.
- **A click during the reload window did nothing** — expected, rarely: the
  text bridge is **at-most-once observation** by contract. During a reload
  dirty window a click frame's bridge POST holds up to the 20s bridge
  timeout, and past the queue bound the oldest queued text is dropped and
  counted (`bridge_dropped`). For this demo that is acceptable and worth
  demonstrating awareness of. Production apps where every command must
  land send commands as plain HTTP requests and use the hub for the
  fan-out — the contract's non-goals state it: "the text bridge is
  at-most-once observation; commands that must reach the server reliably
  are HTTP requests."

## Design notes

Choices this demo makes, and why — adjust with eyes open:

1. **Clicks travel as WSS frames** — the text bridge is the demo's star,
   so interactions ride the socket. The control-plane publish endpoint is
   mentioned but kept off the critical path.
2. **`--workers 1`** — the tally and `STEP` live in one worker's memory
   for demo clarity. A production app externalizes that state, then
   `--workers N` with least-conn selection applies unchanged.
3. **App-id discovery is lazy**: `GET /1.0/apps`, match by `name`, cache,
   re-resolve on snapshot 404. The manager passes no app id to workers in
   env (verified in `spawnWorker`), so list-and-match is the cleanest real
   mechanism. It is needed only for the snapshot and plumbing acts — the
   counter loop itself never needs it.
4. **The open-response greeting is one list frame:**
   `[{"+":["/stats/tally"]}, {"tally!":{"value":N}}, {"@":["/stats/tally"],"viewers!":{"count":M}}]`
   — join, loopback greeting (`@` absent → originating connection, `!`
   mandatory), then the viewer fan-out against post-mutation membership.
   The viewer count is snapshot + 1 because the connection registers only
   after the open answer; concurrent opens can transiently undercount
   (demo-scale, accepted).
5. **The tally persists in `tmp/tally`** inside the app dir — required for
   the 3 → 5 → 7 finale, because a hot reload scraps worker module state.
   `tmp/` is watcher-ignored and hash-ignored (verified in `manager.rip`),
   so the writes are reload-safe.
6. **`bridge_path` is wired by an operator PATCH** after launch — the
   `@rip-lang/server` manager does not register or manage `bridge_path`
   (verified: `registerApp` sends only `name` and `hosts`). Re-PATCH under
   the new id if the manager ever re-registers.
7. **The arithmetic is deliberate**: four increments (1, 2, 3, 4), one
   decrement (3), then step 2 and two increments (5, 7) — a
   self-consistent sequence that lands the 3 → 5 → 7 finale.
8. **Host `demo.ripdev.io`** rides the janus repo's committed trusted
   wildcard certs (`certs/ripdev.io.*`, `*.ripdev.io` → 127.0.0.1).
9. **Cache is on with `debug`** so act 4's verdict is visible; the
   1s-stale inlined tally on `GET /` is accepted because the open-bridge
   greeting corrects it immediately; `/plumbing` opts out with
   `Cache-Control: no-store`. The doorkeeper means the first HIT appears
   on the **third** rapid request (verified in `cache_test.go`) — the act
   4 walkthrough says so.
10. **`tally!` / `viewers!` (include-sender) on every bridge response** —
    required, not stylistic: bridge responses execute in the originating
    connection's exclusion context. On the publish plane the spelling
    would not matter.
11. **Every act is verified against shipped code; none is aspirational.**
    Kick-reaches-the-clicker is confirmed in `hub.go` (kick recipients
    resolve from `@`; sender exclusion applies to events only). The
    snapshot shape is confirmed in `control_hub.go`/`hub.go`.
    Edge-answered `?`/`!` and its bridge observation are confirmed in the
    hub contract and its acceptance sketch.
