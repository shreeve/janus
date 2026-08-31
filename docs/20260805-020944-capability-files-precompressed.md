# Capability 6 extension: precompressed file sidecars

`files` transparently selects precompressed representations for canonical
files in dynamically registered roots. A client requests `/bundle.json`;
Janus may serve `bundle.json.br`, `bundle.json.zst`, or `bundle.json.gz`
without changing the URL.

## Scope and cascade

- Capability order: **6**, extending files without creating a new
  capability number.
- Files admission remains site-scoped and cascades: site override → global
  default → built-in `off`.
- The sidecar format set and preference are process-wide cold policy. They
  are legal only in the global `janus` block. A site may turn inherited
  files service on or off but cannot change its sidecar formats.
- Hot registrations continue to provide ordered roots, root cache policy,
  `proxy_first`, and the SPA shell. Hot JSON cannot enable sidecars or
  change their order.

```caddyfile
{
	janus {
		files {
			precompressed
		}
	}
}
```

Bare `precompressed` enables `br`, `zstd`, and `gzip`, in that server
preference order. An explicit nonempty list enables exactly its formats and
uses its order to break equal client quality, matching Caddy's
`file_server` grammar:

```caddyfile
{
	janus {
		files {
			precompressed br zstd gzip
		}
	}
}
```

The existing `files`, `files on`, and `files off` shorthands remain legal.
A block implies `on`; `files on { ... }` is also legal. `files off` never
takes a block. `precompressed` is global-only, occurs once, and accepts only
`br`, `zstd`, and `gzip`; unknown and duplicate formats reject.

## Ownership and Caddy reuse

Caddy's `fileserver.FileServer` owns its configured filesystem, filename
rewrites, index discovery, and browse behavior. Its sidecar-opening loop is
private and begins after those decisions. Janus cannot delegate one selected
hot registration root to that loop without re-running or bypassing Janus's
tenant gate, root priority, SPA fallback, browse renderer, and descriptor-
relative `os.Root` path jail.

Janus therefore owns canonical-file and sidecar opening. It directly reuses
Caddy's exported `encode.AcceptedEncodings` function for client q-value and
server-preference ordering. Caddy returns a wildcard as the literal `*`;
Janus expands that position into configured formats not explicitly named by
the request, so an explicit `q=0` exclusion wins over `*`. Janus does not
implement a separate ordinary q-value sorter.

## Selection

Existing request order is unchanged: ping, auth, path validation, tenant
resolution, Hub interception, `proxy_first`, and then registered files.
For each file response Janus:

1. opens and stats the canonical requested file using the existing ordered
   roots and path jail;
2. commits to that exact root as soon as it finds a regular canonical file;
3. applies browse renderer selection first when the root is browsable;
4. walks the acceptable representation order;
5. opens `canonical + suffix` only beneath the committed `os.Root`;
6. serves the first regular sidecar that opens and stats successfully, or
   the already-open canonical descriptor when none does.

A sidecar is never consulted until its canonical file exists as a regular
file. A sidecar in a later root cannot supplement a canonical file from an
earlier root. Missing, non-regular, unreadable, or concurrently removed
sidecars are representation misses and safely fall back to the canonical
descriptor. Sidecar failure never causes a 403, 404, or 5xx response.

The same selection applies to a regular file, a discovered directory index,
and the SPA shell. A configured browse renderer still wins for its canonical
extension; Janus does not feed compressed bytes to a renderer. Persistent
cold browse roots do not inherit the registered-files sidecar policy.

The browser-visible URL stays canonical. An explicit request whose path ends
in `.br`, `.zst`, or `.gz` is an ordinary canonical-file request; Janus does
not create or redirect such URLs.

## Negotiation

The configured format order defaults to `br`, `zstd`, `gzip`. Client
q-values sort first and configured order breaks equal quality. Encoding names
are case-insensitive.

- An explicit coding at `q=0` is unavailable, including when `*` has positive
  quality.
- `*` expands at its Caddy-ranked position to configured codings not named
  explicitly anywhere in the field value.
- An explicit positive `identity` participates in the ordered candidates. If
  it sorts before every usable sidecar, Janus serves the canonical file.
- `identity;q=0` does not select the canonical representation, but if no
  acceptable sidecar exists Janus still follows the files fallback contract
  and serves the canonical file rather than manufacturing `406`.
- An absent `Accept-Encoding`, no configured sidecars, or no usable accepted
  sidecar selects identity.

## Representation response

The canonical root supplies the configured `Cache-Control` policy and the
canonical filename supplies `Content-Type`. A selected sidecar supplies its
bytes, size, modification time, and representation ETag. Encoded ETags carry
the coding name in addition to descriptor modification time and size, so a
validator for `br` cannot validate `gzip` or identity even if publishers give
the files identical metadata.

When sidecar policy is configured, file responses carry
`Vary: Accept-Encoding` for both encoded and identity selections. This keeps
shared caches correct across client capabilities and across a temporary
sidecar publication gap. A selected sidecar additionally carries exactly one
`Content-Encoding`. That header prevents Caddy's later `encode` middleware
from compressing the response again.

`http.ServeContent` evaluates `If-None-Match`, `If-Modified-Since`, GET, HEAD,
and ranges against the selected descriptor and its metadata. Janus sets the
selected sidecar's `Content-Length` before non-range service because Go does
not infer it when `Content-Encoding` is present. For `Range`, Janus follows
Caddy's current precompressed file-server behavior: the range addresses the
selected encoded representation bytes, `http.ServeContent` returns the
corresponding `206`, and it computes the partial length.

## Hard errors

- `precompressed` in a site-level `janus` block;
- `precompressed` beneath `files off`;
- duplicate `precompressed` in one files block;
- duplicate format in one format list;
- any format other than `br`, `zstd`, or `gzip`;
- any unknown files-block subdirective or nested block.

## Non-goals

- On-the-fly compression; Caddy's `encode` handler owns that behavior.
- bzip2 or `.bz` sidecars.
- Hot registration of formats, suffixes, or preference.
- Cross-root sidecar lookup.
- Hiding explicitly requested sidecar filenames.

## Acceptance

With regular `bundle.json` and `bundle.json.br` in the same selected root,
`GET /bundle.json` carrying `Accept-Encoding: br` returns the Brotli bytes as
`Content-Encoding: br`, `Vary: Accept-Encoding`, and
`Content-Type: application/json`. A Brotli-capable client decodes the body to
the exact canonical content while its URL remains `/bundle.json`.
