# Capability 6: files

`files` lets Janus serve registered app files before selecting a worker.
Rip Server remains the single writer: an app registration declares its
host admission, ordered roots, worker-first prefixes, and SPA shell.
Janus knows paths and HTTP routing only; it does not know Rip modules,
bags, routes, or application semantics.

## Scope and cascade

- Capability order: **6**, after auth.
- Scope: site-scoped.
- Cascades: **yes** — site override → global default → built-in `off`.
- Cold Caddyfile decides whether file service is active on a site.
- Hot `/1.0` registration supplies each app's host and file policy.
- The process-wide control listener does not choose a Caddy site, so it
  stores valid hot policy regardless of the cold switch. `files off`
  immediately bypasses static service after a reload; `files on`
  reactivates the retained declaration. The Caddy host matcher remains
  the outer admission gate.

```caddyfile
{
	janus {
		files
	}
}

example.com {
	janus
}

api.example.com {
	janus {
		files off
	}
}
```

Legal forms are `files`, `files on`, and `files off`. Arguments,
blocks, duplicates in one `janus` block, and other values reject.

## Hot registration

Ordinary exact-host registration remains unchanged:

```json
{
  "name": "shop",
  "hosts": ["shop.example.com"]
}
```

A directory-gated tenant app registers:

```json
{
  "name": "medlabs",
  "site": {
    "host": "{site}.medlabs.health",
    "dir": "/srv/medlabs/sites",
    "aliases": {
      "localhost": "ola",
      "local.medlabs.health": "ola"
    }
  },
  "files": {
    "roots": [
      { "path": "/srv/medlabs/sites/{site}/app", "class": "live" },
      { "path": "/srv/medlabs/sites/common/generated", "class": "generated" },
      { "path": "/srv/medlabs/sites/common/public", "class": "mutable" },
      { "path": "/srv/medlabs/sites/common/releases/v42", "class": "versioned" }
    ],
    "proxy_first": ["/api"],
    "shell": "/srv/medlabs/app/index.html"
  }
}
```

`hosts` and `site` are alternative admission forms. JSON presence is
strict: `null` is illegal, and nested unknown fields reject. A request using a
pattern or alias resolves only when `dir/<site>` is a real direct child
directory. `common` is reserved and never a site. The capture is one
leftmost DNS label; `{site}` occurs exactly once in `site.host`, whose
only legal shape is `{site}.<valid hostname>`.

The registry owns each pattern suffix exactly as it owns an exact host.
An exact host under an owned suffix conflicts, and a suffix claim
conflicts with every exact host having one or more whole labels before
that suffix. String suffixes without a label boundary do not conflict.
Aliases participate in exact-host conflict checks. Beneath its own
pattern, an alias earns its keep only by REMAPPING: a self-alias whose
host resolves to the same site label the pattern would extract
(`ola.example.com` → `ola` under `{site}.example.com`) is redundant and
rejects, while a remapping alias (`local.example.com` → `ola`) is
legal. Claims are first-wins and are released by DELETE or heartbeat
reap.

`site.dir`, every `files.roots[].path`, and `files.shell` are clean Unix
absolute paths: no NUL, backslash, repeated separator, `.`/`..` segment,
or unsupported template. `site.dir` and `files.shell` never contain a
template. The shell is independent of the roots.
`{site}` may occur once in a root only when `site` is present. Roots are
ordered by declaration and paths are unique. Every root has exactly one
class: `live`, `generated`, `mutable`, or `versioned`. The class selects
the fixed response policy below; there is no arbitrary header surface.
`proxy_first` entries are unique normalized path
prefixes. It is legal to register `files` with exact `hosts` when no root
uses `{site}`. `files` requires nonempty roots and a shell. Missing files
at request time are misses, not registration failures.

`site` and `files` are immutable for one registration. Changing either
uses DELETE followed by POST. PATCH retains its exact-app surface:
name, exact hosts, and bridge path. A site-pattern app rejects a hosts
PATCH. Every routing-changing PATCH bumps the generation under the registry
transaction; a failed PATCH changes no index or record.

## Admission

For host `cheetos.medlabs.health`, Janus:

1. extracts `cheetos`;
2. validates it as one lowercase DNS label and rejects `common`;
3. opens `/srv/medlabs/sites` as a rooted filesystem and stats its direct
   child `cheetos`;
4. admits only a real directory that is not a symlink.

The same lowercase, trailing-dot-free host normalization gates TLS ask,
HTTP and hub lookup. Registration accepts ASCII DNS names only;
HTTP ports are removed, malformed authorities and IP literals do not
match a site pattern. The same resolution gates TLS ask and the HTTP data plane. Wildcard DNS
alone never authorizes a tenant. Creating the direct child admits it;
removing the child withdraws it without a control-plane write.

An alias resolves to its mapped site label, applies the same direct-child
gate, and injects that mapped value. TLS ask performs one direct-child
metadata lookup and never scans the site directory.

Exact hosts resolve before patterns. Registry overlap rejection means
that precedence does not permit one app to shadow another.

## Request order

After ping and auth, Janus validates the request path unconditionally,
then resolves the host and derives the request's site. The same path gate
applies when files is off or absent. Hub interception receives the trusted
site context. With effective `files on`, the data-plane order is:

1. malformed request target → **400**; unknown or
   directory-gate-failed host → **404**;
2. `proxy_first` prefix → existing doorbell/worker path;
3. GET or HEAD regular-file hit in the first ordered root → serve it;
4. GET or HEAD HTML navigation miss → serve `shell`;
5. every other miss or method → **404**.

The files decision is terminal outside `proxy_first`. Only configured API
prefixes reach a doorbell or workers. A missing asset never rings
the manager, and an empty-upstream App-only registration still serves its
files and shell while API prefixes answer 503. Methods other than GET and
HEAD outside `proxy_first` answer 404.

A prefix matches itself and descendants only: `/api` matches `/api`
and `/api/users`, never `/apian`.

HTML navigation means GET or HEAD whose parsed media ranges give
`text/html` positive quality. An absent or malformed `Accept`, or an
explicit `q=0`, does not qualify. A shell is never served for a
`proxy_first` path. A missing shell is a 404.

Static responses set a weak ETag from the opened file's modification
time and size, then use Go `http.ServeContent` conditional and
single-range semantics. GET and HEAD carry the same headers; HEAD emits
no body. The file descriptor supplies both metadata and bytes, so a
path replacement cannot change the served object mid-response. Janus
does not list directories.

Response policy is deliberately finite:

| Source | `Cache-Control` | Content type |
| --- | --- | --- |
| `.rip` from a `live` root | `no-store` | `text/plain; charset=utf-8` |
| `generated`, `mutable`, and other `live` files | `no-cache` | Go's extension MIME result, with CSS and HTML carrying UTF-8 text types |
| Shell | `no-cache` | `text/html; charset=utf-8` |
| `versioned` root | `public, max-age=31536000, immutable` | Go's extension MIME result |

Every file response carries exactly one class policy plus the weak ETag.
The class is registration-time trusted policy, not inferred from a URL
name. `.rip` is forced to plain UTF-8 text and never served as an
executable JavaScript MIME type.

## Path jail

Janus validates both `EscapedPath` and the exactly-once decoded `Path`
before every branch, including `proxy_first` and worker fallthrough. It
rejects malformed escapes, encoded slash or backslash, NUL, backslash,
and `.`/`..` segments. `proxy_first` entries use the same decoded path,
are segment-boundary prefixes, and reject overlap; `/` is legal and
captures every path.

Janus serves regular files only. It uses Go's descriptor-relative
`os.Root` operations, which prevent filesystem races from escaping the
opened root even through symlink replacement. The tenant gate is one
direct-child stat beneath the opened site root. Candidate metadata and
bytes come from the same opened descriptor. Symlinks may resolve only
within the rooted tree; an escape, missing file, directory, device, or
socket is a miss, never a worker-visible filesystem error.

Registration paths are trusted control-plane input but still validate
structurally. Janus never creates a tenant directory.

## Trusted site context

Janus removes every client-supplied `Rip-Site` header at handler entry,
before ping and auth. A resolved tenant request receives exactly
one `Rip-Site: <site>` header. Ordinary exact-host apps receive none.
Hub bridge snapshots and synthetic bridge requests preserve only this
trusted value.

Rip exposes this request-local value as `req.site` / `@req.site`.
There is no process environment variable: one worker may concurrently
serve requests for different sites.

## Lifecycle

App clone/list/get deep-copy `site`, alias maps, roots, and prefixes.
Resolved records are immutable snapshots; filesystem work never holds
the registry lock. DELETE and
TTL reap release exact, alias, and suffix claims. A request resolved before
a concurrent DELETE may complete from
its immutable snapshot, matching the existing in-flight worker rule;
no later request resolves the retired claim.

A Janus restart empties all declarations. Rip Server re-registers the
same normalized policy and resumes heartbeats.

## Hard errors

- both `hosts` and `site`, or neither;
- malformed host template or more than one capture;
- invalid/reserved alias site;
- exact, alias, or suffix ownership overlap;
- relative, duplicate, malformed, or escaped configured paths;
- a root without exactly `path` and `class`, or a root class outside
  `live | generated | mutable | versioned`;
- `{site}` in a root without a site declaration;
- missing shell or empty roots when `files` is present;
- malformed or overlapping `proxy_first` entries;
- unknown JSON or Caddyfile fields.

## Non-goals

- Directory creation, tenant provisioning, or DNS management.
- Rip module, route, apply, or bundle semantics.
- Directory browsing.
- A second configuration language inside Janus.
- Production release channels or filesystem watching.

## Measurement

The full request-construction and file-decision paths are benchmarked,
including first-root hit, second-root hit, shell fallback, and static
miss. Five-run raw provenance is
[`20260730-205400-bench-raw-files.txt`](20260730-205400-bench-raw-files.txt).
