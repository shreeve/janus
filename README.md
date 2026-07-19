<p align="center">
  <img src="docs/janus-720w-white.png" alt="Janus Logo" width="360">
</p>

<p align="center">
  <strong>Caddy module: long-lived edge server — TLS admission, dynamic host routing, and a WebSocket hub, driven by a JSON control API.</strong>
</p>

---

Janus is a Caddy module. Caddy provides listeners, HTTP/1–3, TLS, and ACME. Janus provides the inward face: a memory-resident registry and engines driven by the `/1.0` JSON API. Cold Caddyfile config sets capabilities (such as **control** reachability) and which sites admit traffic into Janus; hot `/1.0` calls decide how admitted hosts map to upstreams, health, hub, and certificate allowlisting.

This repository is a Go module. Caddy is a dependency, not a git submodule. A Janus-enabled binary is produced with [xcaddy](https://github.com/caddyserver/xcaddy), which compiles stock Caddy together with this module into one static `caddy` binary.

**License:** Apache License 2.0 (same family as Caddy’s source).

## Requirements

- **Go** — current stable release ([go.dev/dl](https://go.dev/dl/))
- **xcaddy** — builds Caddy with modules
- A `Caddyfile` that loads Janus (repo root)

### Install Go (macOS, Homebrew)

```bash
brew update
brew install go          # or: brew upgrade go
go version               # confirm current stable
```

### Install Go (official tarball)

Follow [go.dev/doc/install](https://go.dev/doc/install). On macOS Apple Silicon, that is typically the `darwin-arm64` archive or `.pkg` from [go.dev/dl](https://go.dev/dl/). Ensure `$(go env GOPATH)/bin` is on your `PATH` so tools installed with `go install` are available.

### Install xcaddy

```bash
go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest
xcaddy version
```

## Ping proof (current tree)

**ping** is a cascading cold capability (default off). The working `Caddyfile` turns it **on** globally, uses a `*.ripdev.io` catchall (inherit on), `on.ripdev.io` (explicit on), and `off.ripdev.io` (explicit off). Hosts use the trusted wildcard cert (→ `127.0.0.1`) from [`certs/`](certs/). See [`docs/20260718-204255-capability-ping.md`](docs/20260718-204255-capability-ping.md).

```bash
export PATH="$(go env GOPATH)/bin:$PATH"

# Resolve module deps (first time / after Caddy bumps)
go mod tidy

# Build caddy + janus into ./bin/caddy
mkdir -p bin
xcaddy build \
  --with github.com/shreeve/janus=. \
  --output ./bin/caddy

./bin/caddy list-modules | grep janus
# → http.handlers.janus
```

### HTTPS on *.ripdev.io:443

`Caddyfile` serves `*.ripdev.io` / `on.ripdev.io` / `off.ripdev.io` on **:443** with the signed cert in `certs/`. DNS always returns `127.0.0.1`; Caddy picks the site by SNI / Host.

```bash
./bin/caddy run
```

In another terminal:

```bash
curl -s https://foo.ripdev.io/ping          # catchall → pong
curl -s https://on.ripdev.io/ping           # explicit on → pong
curl -s -o /dev/null -w '%{http_code}\n' https://off.ripdev.io/ping
# → 404
```

On some systems binding :443 needs elevated privileges (`sudo ./bin/caddy run …`). On current macOS it often works without sudo.

## Build and run

From this repository (local module replacement is automatic when you run `xcaddy` inside the module):

```bash
# Develop: build a temporary caddy+janus and run it
xcaddy run

# Produce a binary (see ping-only proof above)
xcaddy build \
  --with github.com/shreeve/janus=. \
  --output ./bin/caddy
./bin/caddy run
```

From anywhere, against a published module version:

```bash
xcaddy build \
  --with github.com/shreeve/janus@main \
  --output ./caddy
```

Pin Caddy and Janus versions for reproducible builds (replace versions as appropriate):

```bash
xcaddy build v2.11.2 \
  --with github.com/shreeve/janus@v0.1.0 \
  --output ./caddy
```

Confirm the module is linked:

```bash
./bin/caddy list-modules | grep janus
```

## Layout

| Path | Role |
| --- | --- |
| `app.go` | Process-wide `janus` app (control, global defaults) |
| `handler.go` | Site `http.handlers.janus` (admission + site overrides) |
| `settings.go` | Cascade helpers (`ping on` / `ping off`) |
| `Caddyfile` | Working cold config (multi-site cascade demos) |
| `test.sh` | High-level acceptance suite (self-contained; not a substitute for `go test`) |
| `docs/` | Design notes, SPEC, capabilities (`YYYYMMDD-HHMMSS-` prefixed) |

## Design notes

See [`docs/`](docs/) for the control-plane sketch and related material. The `/1.0` API follows an Incus-inspired style (envelopes, resource paths, ETag on config writes) while remaining Janus’s own protocol.

## Name

In Roman myth, **Janus** is the god of doorways and thresholds — beginnings, passages, and the space between inside and outside. He is shown with two faces: one looking out, one looking in. That is the shape of this module. One face serves the public world over TLS; the other coordinates private upstreams, registry, and control-plane state so that serving is possible. The passage between them is the product.
