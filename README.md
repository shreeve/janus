<p align="center">
  <img src="docs/janus-720w-white.png" alt="Janus Logo" width="360">
</p>

<p align="center">
  <strong>Caddy module: long-lived edge doorway — TLS admission, dynamic host routing, and a WebSocket hub, driven by a JSON control API.</strong>
</p>

---

Janus is a Caddy module. Caddy provides listeners, HTTP/1–3, TLS, and ACME. Janus provides the inward face: a memory-resident registry and engines controlled only by the `/1.0` JSON API over a unix socket. Cold Caddyfile config decides which traffic is admitted into Janus; hot `/1.0` calls decide how admitted hosts map to upstreams, health, hub, and certificate allowlisting.

This repository is a Go module. Caddy is a dependency, not a git submodule. A Janus-enabled binary is produced with [xcaddy](https://github.com/caddyserver/xcaddy), which compiles stock Caddy together with this module into one static `caddy` binary.

**License:** Apache License 2.0 (same family as Caddy’s source).

## Requirements

- **Go** — current stable release ([go.dev/dl](https://go.dev/dl/))
- **xcaddy** — builds Caddy with modules
- A Caddyfile that loads Janus (see `Caddyfile.example` when present)

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

## Ping-only proof (current tree)

This tree ships a **ping-only** Janus: `GET /ping` → `pong`. Enough to prove the module links, Caddyfile admission works, and HTTPS serves on localhost.

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

### HTTPS on localhost:443

Yes — `Caddyfile.example` binds `https://localhost` (port **443**) with `tls internal` (Caddy’s local CA).

```bash
./bin/caddy run --config Caddyfile.example
```

In another terminal:

```bash
curl -sk https://localhost/ping
# → pong
```

On some systems binding :443 needs elevated privileges (`sudo ./bin/caddy run …`). On current macOS it often works without sudo. Curl `-k` skips verifying the local CA for the smoke test; `./bin/caddy trust` installs that CA into the system trust store when you want browsers quiet.

If 443 is unavailable, change the site address to `https://localhost:8443` in `Caddyfile.example`.

## Build and run

From this repository (local module replacement is automatic when you run `xcaddy` inside the module):

```bash
# Develop: build a temporary caddy+janus and run it
xcaddy run --config Caddyfile.example

# Produce a binary (see ping-only proof above)
xcaddy build \
  --with github.com/shreeve/janus=. \
  --output ./bin/caddy
./bin/caddy run --config Caddyfile.example
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
| `janus.go` | `http.handlers.janus` (ping-only for now) |
| `Caddyfile.example` | Cold config: HTTPS localhost → Janus |
| `docs/` | Design notes and API sketches (`YYYYMMDD-HHMMSS-` prefixed) |

## Design notes

See [`docs/`](docs/) for the control-plane sketch and related material. The `/1.0` API follows an Incus-inspired style (envelopes, resource paths, ETag on config writes) while remaining Janus’s own protocol.

## Name

In Roman myth, **Janus** is the god of doorways and thresholds — beginnings, passages, and the space between inside and outside. He is shown with two faces: one looking out, one looking in. That is the shape of this module. One face serves the public world over TLS; the other coordinates private upstreams, registry, and control-plane state so that serving is possible. The passage between them is the product.
