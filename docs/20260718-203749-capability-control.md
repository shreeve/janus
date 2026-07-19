# Capability: control

Cold Caddyfile capability. Configures **where** Janus’s control plane (`/1.0` JSON API) is reachable.

| | |
| --- | --- |
| **Surface** | Cold config only (Caddyfile) |
| **Cascades** | No — process-wide only |
| **Module** | `janus` app (`app.go`) |
| **Directive** | Global options: `janus { control … }` |
| **Pairs with** | Site handler `janus` (data-plane admission) |
| **Status** | Config parsed, validated, logged — `/1.0` serving is Phase 2 |

## Why it exists

Public traffic hits Caddy → site `janus` → data plane. Tenants and operators need a separate door for registry CRUD, heartbeats, and hub publish. That door is the **control plane**.

**control** answers only: *how do you reach `/1.0`?* Not which apps are registered (that is hot `/1.0`).

## Scope

| Piece | Where | Role |
| --- | --- | --- |
| `janus { control … }` | Global options `{ }` | Process-wide control listeners |
| `janus` | Site block | Admit this site’s HTTP into Janus |

One process, one registry. Do **not** nest `control` under a site `janus` block — that would fake per-site control planes.

## Modes

| Mode | Meaning | Listen |
| --- | --- | --- |
| `internal` | Unix domain socket | Optional path (default `run/janus.sock`) |
| `local` | Loopback HTTP | Optional `host:port` (default `127.0.0.1:7600`; host must be loopback) |
| `public` | Explicit network HTTP | **Required** `host:port` — no bare `public` |

Same `/1.0` API on every listener. Multiple modes are allowed (e.g. `internal` + `local`). Duplicate modes are rejected.

`public` without authentication is a footgun; auth is a separate concern (not this capability yet). Prefer `internal` or `local` until then.

## Syntax

```caddyfile
{
	auto_https disable_redirects

	janus {
		control local
		# control internal
		# control internal /var/run/janus/control.sock
		# control local 127.0.0.1:7600
		# control public 192.168.1.10:7600
	}
}

https://localhost {
	tls internal
	janus
}
```

### Legal lines

```text
control internal
control internal <socket-path>
control local
control local <host:port>    # host ∈ {127.0.0.1, ::1, localhost}
control public <host:port>   # address required
```

### Defaults

| Mode | Default listen |
| --- | --- |
| `internal` | `run/janus.sock` |
| `local` | `127.0.0.1:7600` |
| `public` | — (must supply address) |

### Hard errors

- No `control` lines
- Unknown mode
- Duplicate mode
- `control public` without `host:port`
- `control local` with a non-loopback host
- Nested block under `control`
- Site-level `janus { … }` block (control is global only)

## Examples

### Local curl (dev)

```caddyfile
{
	janus {
		control local
	}
}
```

After Phase 2 serves `/1.0`:

```bash
curl -s http://127.0.0.1:7600/1.0
curl -s http://127.0.0.1:7600/1.0/health
```

### Internal only (prod-shaped)

```caddyfile
{
	janus {
		control internal
	}
}
```

```bash
curl -s --unix-socket run/janus.sock http://janus/1.0
```

### Internal + local (socket for Rip, loopback for humans)

```caddyfile
{
	janus {
		control internal
		control local
	}
}
```

## Verify (today)

Config is live even before `/1.0` is served:

```bash
./bin/caddy adapt --config Caddyfile
# → apps.janus.control includes {"mode":"local",…}

./bin/caddy run
# log: janus control configured  mode=local  listen=127.0.0.1:7600

curl -sk https://localhost/ping
# → pong  (data plane unchanged)
```

## Non-goals

- Hot registry / apps CRUD (that is `/1.0`)
- TLS for control listeners (may come later; `local`/`internal` are plain HTTP/unix)
- Authn/authz for `public`
- Changing Caddyfile admission from `/1.0`

## Related

- Build SPEC: [`20260718-191425-janus-build-spec.md`](20260718-191425-janus-build-spec.md) (Phase 2)
- API sketch: [`20260718-182420-janus-api-1.0.html`](20260718-182420-janus-api-1.0.html)
- Working cold config: [`../Caddyfile`](../Caddyfile)
