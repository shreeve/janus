# Capability: control

Cold Caddyfile capability. Configures **where** Janus’s control plane (`/1.0` JSON API) is reachable, and **how** each listener authenticates.

| | |
| --- | --- |
| **Order** | **2** (after primordial **ping**) |
| **Surface** | Cold config only (Caddyfile) |
| **Cascades** | No — process-wide only |
| **Module** | `janus` app (`app.go`, `control.go`, `control_api.go`) |
| **Directive** | Global options: `janus { control … }` |
| **Pairs with** | Site handler `janus` (data-plane admission; **ping** already proven) |
| **Status** | Listeners serve the full hot API: meta, health, apps CRUD, upstreams, heartbeats, tls/ask, cache counters |

## Why it exists

**ping** proved the chassis. **control** is next: open the process-wide door for registry CRUD, heartbeats, and hub publish.

Public traffic still hits Caddy → site `janus` → data plane. Tenants and operators use this separate door for the hot API.

**control** answers: *how do you reach `/1.0`, and with what auth?* Not which apps are registered (that is hot `/1.0` CRUD).

## Scope

| Piece | Where | Role |
| --- | --- | --- |
| `janus { control … }` | Global options `{ }` | Process-wide control listeners |
| `janus` | Site block | Admit this site’s HTTP into Janus |

One process, one registry. Do **not** nest `control` under a site `janus` block — that would fake per-site control planes.

## Listen model

| Config | Result |
| --- | --- |
| No `control` lines | Default: one implicit `control internal` (`run/janus.sock`) |
| One or more explicit lines | **Exactly** those listeners (e.g. only `local` ⇒ no UDS) |

Each line is self-contained: mode, optional listen target, optional `token:…` on the **same** line. There is no sibling `token` directive.

## Modes

| Mode | Meaning | Default listen | Auth |
| --- | --- | --- | --- |
| `internal` | Unix domain socket | `run/janus.sock` | Optional `token:…` |
| `local` | Loopback HTTP(S) | `http://127.0.0.1:7600/` | Optional `token:…` |
| `public` | Network HTTPS | `https://0.0.0.0:7601/` | **Required** `token:…` (env or file only) |

Same `/1.0` API on every listener. Multiple modes are allowed (e.g. `internal` + `local`). Duplicate modes are rejected.

- **local** host must be loopback (`127.0.0.1`, `::1`, or `localhost`).
- **public** must be `https://` (never bare `http://`).
- TLS listeners (public, or an `https://` local) serve `cert:…`/`key:…`
  when given, else the committed dev pair `certs/ripdev.io.{crt,key}`
  (same material as the data-plane demo; see `certs/README.md`).

## Syntax

```caddyfile
{
	auto_https disable_redirects

	janus {
		control internal
		control local
		# control internal /var/run/janus/control.sock
		# control local http://127.0.0.1:7600/
		# control local http://127.0.0.1:7600/ token:JANUS_TOKEN
		# control public token:JANUS_TOKEN
		# control public https://0.0.0.0:7601/ token:./secrets/janus.auth
		# control public token:JANUS_TOKEN cert:/etc/janus/tls.crt key:/etc/janus/tls.key
	}
}

*.ripdev.io {
	tls certs/ripdev.io.crt certs/ripdev.io.key
	janus
}
```

### Legal lines

```text
control internal
control internal <socket-path>
control local
control local <http(s)://host:port[/path]>
control local … token:ENV
control local … "token:literal-secret"
control public
control public <https://host:port[/path]>
control public … token:ENV
control public … token:./path/to/file
control public … token:ENV cert:/path/tls.crt key:/path/tls.key
```

### `token:…` rules

| Form | Meaning |
| --- | --- |
| `token:NAME` (unquoted, no `/`) | Read secret from environment variable `NAME` |
| `token:…/…` (unquoted, contains `/`) | Read secret from file (trailing newline stripped) |
| `"token:…"` (entire arg quoted) | Literal secret — **not allowed on `public`** |

Prefer env names without `$` (avoids clashing with Caddy `{$VAR}` expansion). Auth is HTTP `Authorization: Bearer <secret>` when a token is configured.

### `cert:…` / `key:…` rules

| Form | Meaning |
| --- | --- |
| `cert:<path>` | TLS certificate file for this listener |
| `key:<path>` | TLS private key file for this listener |

Both or neither — one without the other is a hard parse error. Only meaningful on a TLS listener (`public`, or a `local` with an `https://` listen); a plain-HTTP or unix listener rejects them loudly. Unset, a TLS listener uses the committed dev pair `certs/ripdev.io.{crt,key}` (see `certs/README.md`). The pair loads at Start — a missing or unreadable file refuses the config.

### Defaults

| Mode | Default listen |
| --- | --- |
| `internal` | `run/janus.sock` |
| `local` | `http://127.0.0.1:7600/` |
| `public` | `https://0.0.0.0:7601/` |

Port **7601** for public avoids clashing with local **7600**.

### Hard errors

- Unknown mode
- Duplicate mode
- Nested block under `control`
- Sibling `token` directive (must be `token:…` on the control line)
- `control local` with a non-loopback host
- `control public` without `token:…`
- `control public` with a quoted literal token
- `control public` with `http://` (https required)
- Unset/empty env or empty token file at provision time
- `cert:…` without `key:…` (or the reverse), an empty `cert:`/`key:` value, or a duplicate of either on one line
- `cert:…`/`key:…` on a non-TLS listener (`internal`, or `local` over plain `http://`)
- Site-level `janus { control … }` (control is global only)

## API surface (served today)

| Method | Path | Body |
| --- | --- | --- |
| `GET` | `{base}/1.0` | `{ "api_version":"1.0", "type":"janus", "ping":…, "control":[…] }` |
| `GET` | `{base}/1.0/health` | `{ "status":"ok" }` |
| `POST` | `{base}/1.0/apps` | register → `201 { "id":"name-xxxxxx" }` |
| `GET` | `{base}/1.0/apps` | list registered apps |
| `GET` | `{base}/1.0/apps/{id}` | one app record |
| `PATCH` | `{base}/1.0/apps/{id}` | update name and/or hosts |
| `DELETE` | `{base}/1.0/apps/{id}` | deregister → `204` |
| `PUT` | `{base}/1.0/apps/{id}/upstreams` | atomic full-list swap |
| `POST` | `{base}/1.0/apps/{id}/heartbeat` | stamp the heartbeat clock → `204` |
| `GET` | `{base}/1.0/tls/ask?domain=…` | on-demand TLS allowance: 200 allow / 404 deny |
| `GET` | `{base}/1.0/cache` | micro-cache counters: process totals + per-app breakdown ([cache capability](20260720-033201-capability-microcache.md)) |

`{base}` is the path from the listen URL (empty for defaults and for `internal`). Unknown paths under `{base}/1.0` are `404`; a known path with the wrong method is `405`.

## Examples

### Local + internal (working root Caddyfile)

```caddyfile
{
	janus {
		control internal
		control local
		ping
	}
}
```

```bash
curl -s http://127.0.0.1:7600/1.0
curl -s http://127.0.0.1:7600/1.0/health
curl -s --unix-socket run/janus.sock http://janus/1.0
curl -s --unix-socket run/janus.sock http://janus/1.0/health
```

### Public (token from env)

```caddyfile
{
	janus {
		control public token:JANUS_TOKEN
	}
}
```

```bash
export JANUS_TOKEN=…
curl -sk -H "Authorization: Bearer $JANUS_TOKEN" https://127.0.0.1:7601/1.0
```

## Verify

```bash
go test ./...
./test.sh

./bin/caddy adapt --config Caddyfile
# → apps.janus.control includes internal + local

./bin/caddy run
# log: janus control listening  mode=local  listen=http://127.0.0.1:7600/
# log: janus control listening  mode=internal  listen=run/janus.sock
```

## Non-goals

- Changing Caddyfile admission from `/1.0`
- Per-site control planes

## Related

- Build SPEC: [`20260718-191425-janus-build-spec.md`](20260718-191425-janus-build-spec.md) (Phase 2)
- API sketch: [`20260718-182420-janus-api-1.0.html`](20260718-182420-janus-api-1.0.html)
- Working cold config: [`../Caddyfile`](../Caddyfile)
