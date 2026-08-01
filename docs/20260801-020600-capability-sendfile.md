# Capability 8: sendfile

`sendfile` lets a proxied application authorize a filesystem object and ask
Janus to deliver it to the original HTTP client. The application makes the
access decision; Janus opens and streams the bytes.

The instruction is the response header:

```http
X-Sendfile: /absolute/path/to/report.pdf
```

This is a generic Janus reverse-proxy feature. It has no Rip-specific
semantics.

## Scope and configuration

- Capability order: **8**, after files.
- Configuration: **none**.
- Default: **always active**.
- Cascade: **none**.
- Scope: every final response from a registered application upstream that
  would otherwise become the response to the original HTTP client.

The scope includes API and other proxy-first requests, every request method,
and cache fills. It excludes control-plane calls, doorbell rings, Hub bridge
exchanges, heartbeats, health probes, Janus-generated responses, and files
Janus chose without proxying.

Janus interprets only response headers. A client request carrying
`X-Sendfile` has no effect and is removed before the upstream request, so an
application cannot accidentally reflect client input into an instruction.
The same removal applies to client request trailers.

## Trust boundary

A registered application upstream is trusted to select the file. Janus
applies no configured-root jail or URL-to-path mapping. A valid instruction
may name any regular file that Janus can open.

Deployments maintain this invariant:

> Janus has no broader filesystem authority than the application processes
> allowed to issue `X-Sendfile`.

The capability does not make an inaccessible file accessible. A deployment
that gives Janus broader read authority also gives registered applications a
way to exercise that authority through this protocol.

The application authorizes the object to which the pathname resolves when
Janus opens it. Symlinks and path-component changes between application
authorization and that open therefore select the object visible at open
time. Deployments that require a stronger identity guarantee pass a stable
path under their own filesystem policy.

## Instruction

`X-Sendfile` has exactly one field value containing one UTF-8 filesystem
pathname. The value must:

- be nonempty;
- be valid UTF-8;
- contain no NUL;
- be an absolute path on Janus's host.

Janus accepts the instruction only when
`len(resp.Header.Values("X-Sendfile")) == 1`; it never splits a value at
commas. The pathname is literal header text after Go's HTTP whitespace
normalization, not a URL and not percent-decoded.
Spaces, commas, Unicode, `.` and `..` segments, and symlinks have their
ordinary operating-system meaning. More than one field value rejects; Janus
never guesses how to combine multiple instructions.

After recognizing an instruction, Janus closes and discards the upstream
response body without draining it. An instruction response carrying a body
may therefore forfeit reuse of that upstream connection; an unbounded body
can never hold file delivery open. Janus opens the pathname with
`O_RDONLY | O_NONBLOCK | O_CLOEXEC` on Unix and the ordinary read-only file
open on Windows, obtains metadata from that same descriptor, and serves only
a regular file. Descriptor-based metadata and reads ensure that a later path
replacement cannot change the opened object.
A directory, device, socket, pipe, missing file, malformed instruction, open
failure, or metadata failure produces **502 Bad Gateway** with
`Cache-Control: no-store`.

Janus logs the failure as an invalid upstream sendfile response without
returning the filesystem pathname or operating-system error to the client.
The application process remains healthy: a bad file instruction is an
application response error, not a failed upstream connection.

## Response transformation

The upstream response status remains the representation status. A
body-forbidden status (`1xx`, `204`, `205`, or `304`) carrying `X-Sendfile`
is an invalid instruction and produces 502. A `101` upgrade is likewise
invalid and does not affect upstream health. An upstream `206` or `416` is
also invalid: Janus, not the instruction response, owns range selection and
the corresponding status and `Content-Range`.

For an upstream **200**, normal HTTP file semantics may replace the status:

- a satisfied byte range → **206 Partial Content**;
- a failed precondition → **304 Not Modified** or
  **412 Precondition Failed** as required by HTTP;
- an unsatisfiable range → **416 Range Not Satisfiable**.

For another body-capable status, Janus serves the full representation with
that status and does not apply conditional or range status changes.

`GET` streams the selected representation. `HEAD` computes the same status
and headers but emits no body. Other methods may receive the full file body;
range and conditional GET behavior is limited to the methods defined by
`net/http`. Every bodyless outcome closes the file before returning from
response transformation. Every streaming body owns exactly one descriptor
and closes it on EOF, error, or client cancellation.

Janus matches Go `http.ServeContent` semantics for preconditions, validators,
single and multipart byte ranges, `If-Range`, and `HEAD`. The implementation
selects status, headers, offsets, and a streaming body directly; it does not
run `ServeContent` behind a buffering recorder or producer goroutine.

## Header ownership

`X-Sendfile` is an internal instruction. Janus removes every occurrence from
final headers, informational responses, and trailers before any response
reaches the client, including all error paths.

Application representation policy wins. Janus preserves supplied:

- `Content-Type`;
- `Content-Disposition`;
- `Cache-Control`;
- `ETag`;
- `Last-Modified`;
- other end-to-end application headers.

Janus fills only omitted representation metadata:

- `Content-Type` from the filename extension, then content sniffing;
- a weak `ETag` from the opened file's modification time and size;
- `Last-Modified` from the opened file;
- `Accept-Ranges: bytes`.

One supplied `ETag` must be syntactically valid. One supplied
`Last-Modified` must parse as an HTTP date. A malformed or repeated supplied
validator makes the instruction invalid. The effective modification time is
the supplied `Last-Modified` when present and the opened file's modification
time otherwise; Janus uses that same value for output and every date
precondition.

Janus owns transport- and selection-dependent metadata. It discards the
instruction response's `Content-Length`, `Content-Range`,
`Transfer-Encoding`, and trailers, then derives the final response's status,
length, range headers, transfer framing, and body from the opened file.
Transformation atomically replaces the Go response's `StatusCode`, `Status`,
`Body`, `ContentLength`, `TransferEncoding`, `Trailer`, and corresponding
headers; no stale upstream framing survives.

An application-supplied validator is authoritative even when it does not
describe the file metadata. Applications that supply validators are
responsible for changing them when the representation changes.

When the application supplies `Content-Encoding`, the opened file contains
the already content-coded representation. Ranges and lengths describe those
exact file bytes, and downstream encoders do not encode the response again.

## Cache and encoding

The micro-cache sees the transformed file response, never the empty
instruction body. Existing cache rules apply without a sendfile exception:

- only a final storable 200 may be retained;
- range and conditional statuses are not stored;
- oversized bodies abandon buffering and continue streaming;
- application `Cache-Control`, cookies, `Vary`, and the existing safety table
  decide reuse.

Response encoding runs after the transformation. The encoder sees the final
file metadata and bytes. Existing behavior for an application-supplied
`Content-Encoding` remains authoritative. Janus preserves net/http's
transparent gzip negotiation and decoding for ordinary proxied responses,
but never decodes a final client-bound sendfile instruction: its encoding
describes the selected file rather than the discarded upstream body.

## Processing order

```text
client request
→ Janus admission, routing, and cache decision
→ upstream selection and retries
→ final accepted upstream response
→ inspect and remove X-Sendfile
→ open and validate the file
→ apply validators, ranges, and final headers
→ micro-cache recording
→ response encoding
→ client
```

Marked busy/draining responses rejected for retry are not final and therefore
cannot trigger file delivery. Internal Janus exchanges never enter this
sequence.

## Acceptance

Go tests pin:

- absent and client-supplied headers have no effect;
- one valid absolute UTF-8 path is accepted;
- relative, empty, NUL, invalid UTF-8, duplicate, missing, and
  non-regular targets fail with 502 and never leak `X-Sendfile`;
- the upstream body is discarded;
- supplied representation headers win and omitted metadata is filled;
- instruction transport headers never survive;
- GET, HEAD, validators, single ranges, multipart ranges, and unsatisfiable
  ranges match `http.ServeContent`;
- body-forbidden and upstream range-selection statuses reject; other
  body-capable non-200 statuses retain their status;
- sendfile failures do not mark an upstream unhealthy;
- cache fills record the final file response and obey the existing
  never-store and body-size rules;
- an upstream instruction body is never read and is closed immediately;
- an unconnected FIFO returns under a hard deadline;
- canceled full and multipart responses close their descriptor without
  leaving a readable descriptor; multipart assembly starts no goroutine;
- bodyless outcomes, stale upstream trailers, and stale Go response framing
  leave no resources or metadata behind;
- supplied content encoding, ranges, and lengths describe the same selected
  bytes.

Wire-invalid header bytes rejected by Go's HTTP transport are ordinary
malformed-upstream failures because no response reaches the transformation.
Synthetic unit tests still pin the validator's NUL and invalid-UTF-8
rejection. Informational `1xx` responses other than `101` are consumed by
Go's transport and are not acceptance inputs.

The full Caddy suite proves a real registered upstream can deliver a file,
override metadata, answer HEAD and ranges, fail without leaking the
instruction, and pass the final response through the configured handler
chain.

## Measurement

Benchmarks cover the complete response transformation and body delivery, not
only instruction parsing. The retained raw run compares:

- direct Janus registered-file delivery;
- proxied `X-Sendfile` delivery;
- ordinary proxied file bytes.

Construction, metadata, and streaming costs all count. The benchmark uses a
file larger than the proxy and cache buffers so it measures the streaming
path rather than an in-memory special case.

The five-run 1 MiB benchmark measured median delivery at **50.1 µs** for
registered files, **71.7 µs** for proxied sendfile, and **357.7 µs** for
worker-streamed bytes. Sendfile retained the proxy/authorization trip while
delivering the body **4.99× faster** than worker streaming in this harness.
Raw command, scope, environment, and all runs:
[`20260801-030843-bench-raw-sendfile.txt`](20260801-030843-bench-raw-sendfile.txt).

## Non-goals

- Authorizing the request or choosing the pathname.
- Restricting paths to registered file roots.
- Mapping internal paths to public URLs.
- Directory listing or non-regular-file delivery.
- Configuring sendfile per site or application.
- Defining a Rip response helper.
