# Capability 8: browse

`browse` turns explicitly selected filesystem roots into navigable web
spaces. Janus owns directory discovery, listing presentation, ordinary file
delivery, and optional extension renderers. Rip workers are not required.

The capability preserves the v3 browser's useful surface — breadcrumbs,
metadata, file-type icons, responsive layout, and image previews — while
keeping bytes, validators, ranges, and MIME handling at the edge.

## Scope and ownership

- Capability order: **8**, after sendfile.
- Cold gate: **required**. Browse behavior is inactive unless cold Caddy
  configuration enables it.
- Cascade: **yes** for site browse admission; global default → site override.
- Process-wide settings: theme, extension renderers, renderer defaults, and
  renderer process limits. Site blocks cannot override them.
- Hot policy: `browse: true` on an individual registered file root.
- Cold policy: roots declared directly on a Caddy site.
- Default: off.

Cold configuration decides whether Janus may browse at all and which
executables Janus may launch. Hot registration may select a root for browsing
but can never define a theme, executable, argument, environment variable,
limit, or content type.

## Global Caddyfile grammar

The global `janus` options block accepts one `browse` occurrence:

```caddyfile
{
	janus {
		browse {
			theme /etc/janus/themes/default
			timeout 10s
			max_output 8MB
			concurrency 4

			renderer .md {
				command bun /opt/janus/render-markdown.mjs {file}
				content_type "text/html; charset=utf-8"
				concurrency 2
			}

			renderer .rip {
				command rip --html {file}
				content_type "text/html; charset=utf-8"
				timeout 15s
				concurrency 1
			}
		}
	}
}
```

Legal global forms are `browse` and `browse { ... }`. Presence enables the
global browse default. `browse on`, `browse off`, arguments on the bare line,
duplicates, unknown subdirectives, and nested blocks under leaf directives
reject.

The global block accepts:

- `theme <absolute-directory>` once;
- `timeout <positive-duration>` once, default `10s`;
- `max_output <positive-size>` once, default `8MB`;
- `concurrency <positive-integer>` once, default `4`;
- one or more `renderer <extension> { ... }` blocks.

Sizes use Caddy/go-humanize binary units: `MB` and `MiB` are 1,048,576 bytes.

The embedded default theme is used when `theme` is absent. A configured theme
directory is loaded and validated during Caddy provisioning. Janus never reads
theme files on a request. Editing a theme requires a successful Caddy reload.

An extension is ASCII, starts with `.`, and contains only letters, digits,
`_`, `-`, and additional dots. Janus lowercases it before duplicate
comparison and storage. Exactly one renderer may own an extension
process-wide. Request matching compares the lowercase basename against all
registered keys and takes the longest suffix, so `.tar.gz` wins over `.gz`.
The basename must contain at least one byte before the suffix.

Each renderer requires:

- one `command` line containing an executable plus literal argv tokens;
- exactly one argv token equal to `{file}`;
- one valid `content_type`;
- optional `timeout`, `max_output`, and `concurrency` overrides.

`{file}` cannot be argv zero. Janus invokes argv directly with
`exec.CommandContext`; Janus never constructs a shell command. An operator may
explicitly configure a shell as the executable, but Janus supplies no parsing,
quoting, interpolation, pipes, redirects, globbing, or shell environment.

The executable must be absolute or resolve through `PATH` during provisioning.
An invalid theme, missing executable, malformed MIME type, empty command,
missing or repeated `{file}`, duplicate extension, nonpositive limit, invalid
size or duration, or unknown token rejects the complete Caddy load.

## Site Caddyfile grammar

A site-level `janus` handler accepts:

```caddyfile
janus {
	browse
}
```

Legal site forms are `browse`, `browse on`, `browse off`,
`browse { root … }`, and `browse on { root … }`. `browse off` never accepts a
block. A nonempty block accepts only `root` directives. An empty block means
on. `theme`, `renderer`, and process limits at site scope reject as
process-wide settings.

Effective browse on implies effective files on for browsable roots. An
explicit `files off` at the same site with effective browse on rejects during
provisioning. At global scope, explicit `files off` with browse enabled also
rejects. Browse off with files on keeps ordinary Capability 6 delivery and
disables listings and renderers.

The complete cascade rule is: browse on makes files effective on only when
files is unset at both scopes. Provisioning rejects any site whose effective
browse is on and whose effective files is explicitly off, regardless of which
scope supplied either value. Browse off never changes effective files.

A site may declare persistent cold roots:

```caddyfile
downloads.localhost {
	janus {
		files
		browse {
			root /Users/me/Downloads revalidate
			root /Volumes/Media forever
		}
	}
}
```

Each `root` has an absolute clean path and an optional cache policy
(`never`, `revalidate`, or `forever`; default `revalidate`). Cold roots
are browsable by definition. They are not hot app records, have no app id,
upstreams, heartbeat, Hub, or DELETE endpoint, and are restored from the
Caddyfile after process restart.

Every site containing cold roots must have a finite nonempty set of exact DNS
names in its Caddy `host` matcher. For every route path leading to a
cold-browse handler, Janus intersects host matchers within that path and unions
matcher-set alternatives. Every alternative must resolve to the same finite
nonempty exact-host set. Wildcards, catch-alls, placeholders, IP literals,
ambiguous alternatives, and duplicate hosts across cold browse handlers
reject. Cold roots do not support the hot `{site}` pattern.

Cold hosts occupy the same conflict namespace as hot exact hosts, aliases,
and site-pattern suffixes. A Caddy load that conflicts with a hot claim
rejects; a hot registration that conflicts with a cold claim returns 409
naming the holder as `cold:<host>`. TLS ask admits both hot and cold claims.

At Janus `Start`, one proposed Caddy generation atomically reserves its cold
claims in pooled state before it may serve. A proposed generation may overlap
an active predecessor reservation solely when both reservations have the same
normalized host; handler identity, root set, and source location do not
participate. Duplicate hosts within one generation always reject. Other cold
sites and hot registrations conflict with either reservation. A proposed
claim is visible to TLS ask and may temporarily block a hot POST before Caddy
commits the reload. Successful reload keeps the new reservation and retiring
the old generation releases the old one. An aborted reload's cleanup releases
only its proposed reservations; old routes and claims remain. No request is
routed to a proposed cold root before Caddy activates its handler.

Removing a cold root or site in a successful reload releases its claims.
Aborted reloads leave the serving generation and its claims unchanged.

## Hot registration

Capability 6's file root gains one optional Boolean:

```json
{
  "files": {
    "roots": [
      {
        "path": "/srv/medlabs/public",
        "cache": "revalidate",
        "browse": true
      }
    ],
    "proxy_first": ["/api"],
    "shell": "/srv/medlabs/app/index.html"
  }
}
```

Absent or false means that root remains regular-files-only. `null`, a
non-Boolean, or an unknown field rejects. The registry stores valid browse
policy even where the current Caddy site has browse off; the cold switch
decides whether it is active.

`files.shell` may be omitted only when every root has `browse: true`,
`proxy_first` is empty, and the registration's `upstreams` is empty. This is
a terminal browse-only registration. Publishing nonempty upstreams later
rejects. All other files policies retain Capability 6's required shell.

Files policy remains immutable for one registration. Changing a root's HTTP cache policy,
browse flag, proxy-first prefix, or shell uses DELETE followed by POST.

## Request decision table

Browse runs after request-path validation and auth, inside the files branch,
before SPA shell fallback and before proxy handling:

After ping, site auth runs first. This table applies only after auth permits
the request:

1. malformed request target → **400**;
2. unknown or directory-gate-failed hot host → **404**;
3. a reserved theme asset path → immutable theme asset;
4. `proxy_first` match → existing doorbell/worker path;
5. method other than GET or HEAD → **404**, preserving files behavior;
6. for each ordered root:
   1. a regular-file hit wins;
   2. if browse is cold-enabled and that root is browsable, default view or
      raw delivery is selected;
   3. otherwise a directory hit on a browsable root wins;
   4. a nonmatching or non-browsable directory continues to the next root;
7. a winning directory without a trailing slash → **308** to the same escaped
   path plus `/`, preserving the query;
8. a winning directory checks, in order, `index.html`, `index.rip`,
   `index.ts`, `index.tsx`, `index.jsx`, and `index.js`; a regular index file
   receives the same default-view decision as an ordinary file;
9. a directory with no index → listing HTML;
10. no root match plus an HTML navigation and configured shell → shell;
11. every other miss → **404**.

GET and HEAD use the same 308. `Location` is the original once-escaped
`EscapedPath` plus `/` and, when nonempty, `?` plus the original RawQuery.
Repeated slashes are preserved; URL fragments never reach the server.

Root order remains authoritative. A regular file in an earlier root wins over
a directory in a later root. A browsable directory in an earlier root wins
over every later object. A non-browsable directory is not a match and does not
shadow later roots.

A cold claim skips Hub, `proxy_first`, SPA shell, doorbell, and
worker routing. It applies its ordered cold roots after auth and path
validation; a miss is terminal 404.

## Directory listing

Janus opens each root with `os.OpenRoot`, opens the request-relative directory
through that root, and reads entries from the opened descriptor. URL decoding,
encoded slash/backslash rejection, dot-segment rejection, and root confinement
are exactly Capability 6's rules.

The listing:

- includes every entry, including dotfiles, up to a hard limit of 10,000;
- sorts directories first and files second, each by bytewise name;
- includes parent navigation below the selected root only;
- shows escaped breadcrumbs, name, size, modification time, and type icon;
- previews `png`, `jpg`, `jpeg`, `gif`, `svg`, `webp`, and `avif` by loading
  their ordinary file URL;
- gives renderer-owned extensions a rendered default link and an explicit raw
  action;
- gives all other files their ordinary URL;
- emits only escaped template data and per-segment URL escaping.

Janus reads at most 10,001 names from the directory before any entry metadata
lookup or sorting. More than 10,000 entries returns a terminal generic **503**
with `Cache-Control: no-store`. Rendered listing HTML is capped at 16 MiB by a
bounded chunk writer that never retains more than 16 MiB of output; overflow
returns the same terminal generic 503. Rejected listings do not increment the
listing counter or publish their theme.

Selecting a root is authorization to list everything reachable within that
root. Janus maintains no allowed-root list and does not hide dotfiles,
configuration names, databases, or source files. Choosing `/` authorizes `/`.
Descriptor-relative confinement guarantees that listing and ordinary byte
delivery cannot use path syntax or a symlink race to escape the chosen root.
Renderer execution has the separately documented pathname trust boundary.

Entries use `Lstat` metadata. A symlink is displayed as a symlink, sorted with
files, with its own size and modification time and no preview. Following it
through an ordinary request succeeds only when `os.Root` can resolve the
target within the root; broken and escaping links are misses. Invalid UTF-8
filename bytes are sorted and URL-escaped as their original bytes and shown
with Unicode replacement characters.

Sockets, FIFOs, devices, and other non-file entries use kind `other`, icon
`binary`, and their `Lstat` metadata; URL, RawURL, and PreviewURL are empty.

Listing responses are `text/html; charset=utf-8`,
`Cache-Control: no-store` and `X-Content-Type-Options: nosniff`. GET returns
the page; HEAD computes the same headers and
Content-Length without writing the body.

## Theme

Janus ships ordinary source files embedded with `go:embed`:

```text
browse/
├── index.html
├── browse.css
├── browse.js
└── icons.svg
```

The HTML is a Go `html/template`. Janus installs no custom template
functions; only `html/template`'s built-ins exist. The dot is:

```go
type BrowsePage struct {
    Version     int             // always 1
    Title       string          // "Index of " + Path
    Path        string          // decoded request path
    RootName    string          // configured root base name
    AssetBase   string          // /_janus/browse/<hash>
    Parent      *BrowseLink     // nil at the selected root
    Breadcrumbs []BrowseLink
    Entries     []BrowseEntry
}
type BrowseLink struct {
    Name string
    URL  string
}
type BrowseEntry struct {
    Name       string
    URL        string
    RawURL     string // empty without a renderer
    Kind       string // directory, file, symlink, or other
    Icon       string // directory, image, audio, video, text, archive, binary, or symlink
    Size       int64
    SizeText   string
    Modified   time.Time
    ModifiedText string // UTC RFC3339
    PreviewURL string // empty unless previewable
    Rendered   bool
}
```

Unknown functions are template parse errors. Janus statically walks the parsed
template tree and validates every field chain against the documented model,
including chains inside branches that sentinel data may not execute. Template
invocation pipelines are validated, and each called template is checked
against the pipeline's static dot type rather than the top-level page type. It then
executes the template once against a complete sentinel page. Unknown fields
or either validation failure reject provisioning. CSS, JavaScript, icons, and
any additional regular assets are addressed beneath:

```text
/_janus/browse/<theme-hash>/<asset-path>
```

This prefix is reserved on browse-enabled sites and is handled before app
roots, shells, or upstreams. Asset paths are descriptor-confined,
dot-segment-free, and content-addressed. Theme assets receive
`Cache-Control: public, max-age=31536000, immutable`, a content hash ETag,
correct MIME metadata, and GET/HEAD semantics. A matching `If-None-Match`
returns **304** with the immutable cache and validator headers and no body.

The entire reserved prefix is terminal. Unknown hashes, assets, malformed
asset paths, and methods other than GET or HEAD return 404 and never fall
through to roots, shell, or upstream.

A custom theme directory replaces the embedded theme wholesale. Janus walks
it during provisioning, rejects symlinks and non-regular assets, requires the
four files above, caps total theme bytes at 16 MiB, parses the template, and
computes one deterministic hash over relative paths and bytes. Missing,
unreadable, malformed, or oversized assets reject the
Caddy load.

Each asset's reported size is checked against the remaining total before
allocation. The read is capped through a limited reader and must match that
size exactly, so a single oversized or concurrently growing file cannot force
an allocation beyond the 16 MiB theme-byte budget.

The walk follows no symlink, rejects unreadable paths, sockets, devices, and
other non-regular entries, ignores empty directories, and treats hard links as
separate assets whose bytes each count toward the cap. Relative names use
slash separators and must be valid UTF-8. Exact path identity is
case-sensitive; configuring a case-colliding tree on a case-insensitive
filesystem is rejected by the filesystem before Janus can observe two assets.

Theme and renderer configuration belongs to one Caddy generation. A successful
reload atomically makes the new theme and renderer table visible to new
requests across all hosts. Existing requests retain their generation snapshot.
A failed reload changes nothing.

Provisioning never publishes a theme into pooled state. The serving generation
resolves its own current hash directly and retains that theme only after a
successful listing or valid current-theme asset request reaches an active
handler. Retained themes remain pooled by hash until process exit, so old
successfully used content-addressed URLs remain valid across reload. A rejected
or provisional generation that never handles a successful theme-backed request
leaves no globally addressable hash behind.
The theme hash is the first 24 lowercase hexadecimal characters of SHA-256
over every asset sorted by slash-separated relative pathname, each encoded as
`uint64 big-endian path-byte-length`, path bytes, `uint64 big-endian
content-byte-length`, and content bytes. File modes and modification times do
not participate. The ETag is the quoted full lowercase SHA-256 of that asset's
bytes. MIME type is extension-derived, with explicit UTF-8 types for HTML,
CSS, JavaScript, JSON, and SVG; unknown assets are
`application/octet-stream`. Every asset receives
`X-Content-Type-Options: nosniff`.

## Default file action

The ordinary URL is the best configured browser representation:

- if the winning browsable root's extension has a renderer, plain GET runs it;
- otherwise Janus serves the ordinary file using Capability 6;
- `RawQuery`, split on `&`, containing a component exactly equal to `raw`
  bypasses a renderer and serves ordinary bytes;
- `raw` with a nonempty value and every other query parameter do not change
  selection;
- a raw/download link emitted by the listing preserves existing raw query
  bytes and appends `&raw`, or uses `?raw` when the query is empty.

`raw=`, `raw=x`, percent-encoded key spellings, malformed query components,
and repeated valued occurrences do not themselves bypass. Detection examines
components independently without decoding, so `raw&%ZZ` does bypass because
one component is exactly `raw`. A valueless `raw` among any other components
does. Query parsing never changes request-path validation.

Janus does not inspect `Accept` to guess intent. A configured renderer is the
operator's default for that extension. Native browser types such as audio,
images, video, and PDF remain native unless the operator explicitly configures
a renderer for their extension.

Renderer output is standalone; Janus does not wrap or rewrite it. Janus makes
these variables available to the child in addition to the inherited process
environment:

- `JANUS_BROWSE_FILE` — absolute selected pathname;
- `JANUS_BROWSE_URL` — escaped request path without query;
- `JANUS_BROWSE_RAW_URL` — same path, preserving the request's RawQuery bytes
  and appending `&raw`, or using `?raw` when the original query is empty;
- `JANUS_BROWSE_ROOT` — absolute configured root.

The command's working directory is the selected file's directory. `{file}` is
replaced with the absolute selected pathname as one argv element.

When a directory URL selects an index, the request URL remains canonical:
`JANUS_BROWSE_URL` is the directory path, its raw URL is that directory URL
with `raw`, and raw delivery returns the selected index bytes. The underlying
index filename is exposed only through `JANUS_BROWSE_FILE`.

Renderer input deliberately uses a trusted operator-visible pathname, not the
already-open descriptor used for ordinary delivery. Janus first proves that
the selected object is a regular file reachable through `os.Root`, then
launches the command. A local writer can replace that pathname before the
renderer opens it, and a renderer may follow neighboring paths from its
working directory. Configuring a renderer therefore trusts both the command
and local writers with access to the selected tree. Janus's path confinement
promise applies to listing and ordinary/raw delivery, not to what a configured
child subsequently opens.

Provisioning resolves argv zero once to an absolute executable path and stores
it in the generation snapshot; requests do not consult `PATH` again. Janus
inherits the operator environment but overwrites every `JANUS_BROWSE_*`
variable it defines.

GET captures stdout and returns it only after a successful exit. HEAD never
spawns; it returns the configured content type, no Content-Length, and no body.
Raw GET/HEAD retains Capability 6 validators, ranges, MIME detection, cache
policy, and streaming.

## Renderer bounds and failures

The global concurrency limit counts every renderer child across every host.
Each renderer's optional limit adds a second process-wide limit for that
extension. Admission does not queue:

- global or extension saturation → **503**, `Retry-After: 1`,
  `Cache-Control: no-store`;
- timeout → **504**, `Cache-Control: no-store`;
- nonzero exit, start failure, or oversized stdout
  → **502**, `Cache-Control: no-store`;
- client cancellation terminates and reaps the child and writes no replacement
  response.

Stdout is capped at the effective `max_output` before any response headers are
committed. Stderr is capped at 64 KiB, logged with extension and exit status,
and never returned. Error bodies are generic and never include argv, pathname,
stderr, environment, or operating-system errors.
`max_output` must be small enough for an internal one-byte overflow sentinel;
`MaxInt64` therefore rejects in both Caddy grammar and programmatic
provisioning.

Janus derives a timeout context from both the request and Caddy generation.
Stopping or replacing a generation cancels its renderer children. If headers
remain uncommitted, reload cancellation returns 503 with
`Cache-Control: no-store`; otherwise Janus closes the response. Ordinary,
listing, raw, and theme-asset requests keep their generation snapshot.

On Unix, every child starts in a new process group. Cancellation, timeout, and
overflow send TERM to the group, wait 250ms, send KILL to the group, drain
stdout and stderr concurrently, and call `Wait` before releasing admission
slots. On Windows, Janus kills and waits for the direct child; descendant-tree
termination is not guaranteed. Output overflow beats a simultaneous normal
exit and returns 502; timeout beats cancellation and returns 504; explicit
request or generation cancellation otherwise uses the cancellation behavior
above. Start failure has no exit status.

Rendered success is status 200 with the configured Content-Type,
`Cache-Control: no-store`, and `X-Content-Type-Options: nosniff`. It has no
ETag, Last-Modified, ranges, or compression promise.

One pooled renderer supervisor owns the total running-child count across
overlapping Caddy generations. Admission under one mutex applies the proposed
generation's total limit and its renderer's extension limit against all
currently running children. Lowering a limit below current occupancy cancels
no admitted process; new work remains saturated until occupancy falls below
the new limit. Slots release only after `Wait`.

## Registration and root lifetimes

### Heartbeat lease

The existing default remains `lease: "heartbeat"`. The lease is immutable
after POST. Registration stamps the
clock, heartbeat refreshes it, TTL reap removes it, DELETE removes it, Caddy
reload retains it, and process exit loses it.

`rip server browse <directory>` creates one files-only heartbeat registration,
publishes no upstreams, sends heartbeats while the command lives, deletes on
orderly shutdown, and prints the selected URL. It resolves exactly the named
directory and adds no project, package, generated, public, or App roots.

The command uses the existing required `--control`/`JANUS_CONTROL` discovery.
It accepts optional `--host`; otherwise it generates one
`browse-<12-lowercase-hex-random>.localhost` host. It POSTs name `browse`,
that exact host, empty upstreams, heartbeat lease, and one revalidating browsable
root with no shell. Host 409 generates a new default host and retries up to
five times; an explicit `--host` never changes and a 409 fails. The printed URL
is `https://<host>/`. Invalid or missing directories and unreachable controls
fail before POST.

### Process lease

`POST /1.0/apps` accepts:

```json
{ "lease": "process" }
```

Legal values are `heartbeat` and `process`; omission means heartbeat. `null`,
empty, or another value rejects. A process lease:

- is skipped by heartbeat TTL reaping;
- rejects heartbeat POST with **400**;
- has no heartbeat clock and cannot be changed to another lease;
- is removed by DELETE;
- survives Caddy reload through pooled process state;
- disappears on Janus/Caddy process exit;
- is not reconstructed after restart.

List and get responses include the normalized lease. Existing registrations
without a supplied lease report `heartbeat`.

`rip server browse <directory> --until-restart` creates a process lease,
prints its id, URL, and deletion instruction, and exits without heartbeat or
DELETE.

### Cold Caddyfile roots

Cold roots are Caddy configuration, not registrations. They survive restart
because Caddy reloads them, and they follow Caddy validation/reload lifecycle.
They never appear in `/1.0/apps`, never accept heartbeat or DELETE, and are
included in browse status as cold roots.

`GET /1.0/tls/ask?domain=<host>` keeps its existing success status and body
for hot claims. A cold claim returns 200 with
`{"domain":"<normalized-host>","claim":"cold"}`. Unknown names remain 404.

### Managed app roots

A normal Rip manager may register `browse: true` roots from `serve.rip`.
Those roots share the app's required heartbeat lease, hosts/site policy, auth, hold,
files cache policies, shell, and deletion. They are not separate apps.

## Control and observability

`GET /1.0/browse` returns a redacted process-wide snapshot:

- enabled state and effective embedded/custom theme hash;
- renderer extensions, content types, effective limits, and current running
  counts, but not executable paths or arguments;
- cold browse hosts and root count, but not filesystem paths;
- totals for listings, rendered successes, failures, timeouts, saturation,
  and raw bypasses.

Counters are pooled and survive Caddy reload until process exit. A GET or HEAD
listing increments `listings`. A valueless raw bypass increments
`raw_bypasses` only when it bypasses a configured renderer.
`render_attempts` increments after admission; `render_starts` increments only
after `cmd.Start` succeeds; success increments `render_successes`.

Every admitted request receives exactly one terminal outcome classification.
Start failure, nonzero exit, output overflow, timeout, client cancellation,
and reload cancellation each increment `render_failures`; timeout also
increments `render_timeouts`, output overflow increments
`render_overflows`, and either cancellation increments
`render_cancellations`. The documented timeout-over-cancellation precedence
chooses the sole subtype during a race. A context already canceled before
`cmd.Start` is cancellation, not start failure. One rejected admission
increments `render_saturations` once, regardless of whether both global and
extension limits are full. Status snapshots load each atomic counter
independently and are not a transactional cross-counter snapshot.

Configuration fields in `/1.0/browse` come from the generation serving the
control request. `enabled` means that generation has at least one effective
browse-enabled site. Cold hosts include only that generation's configured
hosts, not another provisional reservation. Running counts include every
pooled child; children from retired renderer definitions appear separately as
`retired_running`.

The `/1.0` metadata names browse support. App JSON includes root browse flags
and lease but no cold renderer or theme data.

## Processing order

```text
Caddy site admission
→ ping
→ auth
→ path validation
→ hot host or cold browse host resolution
→ reserved theme asset
→ hot claim: Hub interception → proxy_first bypass
→ cold claim: ordered cold roots → terminal 404
→ ordered file roots
   → ordinary/raw file
   → renderer
   → directory index
   → listing
→ SPA shell
→ upstream
→ 404
```

Auth runs before browse. A gated root therefore has the same authentication
posture as the rest of its site. Renderer commands never receive
authentication secrets, cookies, request headers, or request bodies through
the environment.

## Acceptance

Go tests pin:

- every legal global/site grammar and every duplicate, unknown, malformed, or
  misplaced token;
- embedded and custom theme loading, deterministic hashes, size bounds,
  template escaping, and failed-reload atomicity;
- strict hot JSON for root browse and lease;
- cold/hot cascade and per-root browse gating;
- ordered roots, index precedence, trailing slash, shell fallback, and method
  behavior;
- dotfiles, Unicode, reserved characters, malformed escapes, traversal, and
  symlink confinement;
- listing sort, breadcrumbs, metadata, preview, rendered/default/raw links,
  HEAD, cache headers, and theme assets;
- ordinary audio/image/PDF MIME delivery with no renderer;
- argv substitution without a Janus-created shell, inherited environment,
  renderer metadata variables, and working directory;
- global and extension concurrency, timeout, cancellation, output/stderr caps,
  nonzero exit, missing executable, and process cleanup;
- heartbeat and process leases, reaping, heartbeat rejection, DELETE, and
  reload survival;
- cold/hot claim conflicts, claim release, TLS ask, and cold reload behavior;
- status redaction and exact counters.

Reload and race pins include failure after Janus `Start`, provisional-claim
cleanup, concurrent hot POST against cold reservation, old/new cold overlap,
old theme assets after reload, and combined old/new renderer concurrency.
Additional table pins cover wildcard/catch-all cold-site rejection,
auth-preempted malformed paths, `?raw`/`?raw=`/valued/repeated/encoded/malformed
query forms, longest multi-dot renderer suffix, invalid-UTF-8 filenames,
index raw URLs, symlink replacement before renderer open as the documented
trusted-path behavior, Unix descendant cleanup, mixed heartbeat/process
sweeps, process heartbeat rejection without mutation, process DELETE claim
release, managed-root reap, cold absence from app endpoints, and the cold
TLS-ask body.

The foreground Caddy acceptance suite proves:

- a hot browsable root lists over HTTPS and previews an image;
- ordinary audio and raw downloads retain file semantics;
- one configured renderer owns its extension across two hosts;
- renderer success, saturation, timeout, and failure statuses;
- site browse off beats global on;
- a heartbeat browse registration reaps;
- a process registration survives heartbeat TTL and Caddy reload, then DELETE
  removes it;
- a cold browse site returns after process restart from the same Caddyfile;
- a cold/hot host conflict rejects;
- a managed tenant root lists while its API prefix still proxies.

## Measurement

Retained raw benchmarks cover:

- regular registered-file delivery with browse absent, false, and true;
- directory listing generation for 10, 100, and 1,000 entries;
- theme asset delivery;
- one no-op renderer process and one representative Bun renderer;
- cold configuration/theme provisioning.

The ordinary file path is measured for construction and full-body delivery;
enabling browse with no directory or renderer hit must not add unexplained
allocation or latency drift. Renderer process startup is documented, not
presented as a high-throughput path.

## Non-goals

- Per-app, per-site, or per-directory themes or renderer definitions.
- Hot control-plane executable configuration.
- Shell syntax supplied or interpreted by Janus.
- CGI response headers, status parsing, request bodies, or request-header
  forwarding.
- Renderer output caching, validators, ranges, or streaming before exit.
- Guessing view intent from `Accept`.
- Protecting an operator from intentionally selecting a broad root.
- Durable storage for hot registrations.
- Worker involvement in listing or ordinary file delivery.
