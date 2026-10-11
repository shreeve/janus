# Confine: a cold allowlist for hot file roots and X-Sendfile targets

Status: **Draft — awaiting owner review.** Amends
[capability 6, files](20260730-202700-capability-files.md) and
[capability 7, sendfile](20260801-020600-capability-sendfile.md). Nothing
here is implemented; the shipped contracts remain authoritative until the
owner rules on the decisions below.

`confine` is one process-wide Caddyfile directive naming the directory
trees that hot `/1.0` file policy and upstream `X-Sendfile` instructions
may resolve into. Everything outside those trees is unreachable through
Janus, whatever a registration or an upstream response names. Enforcement
is descriptor-relative (`os.Root`), never a string-prefix test on a path
the kernel has not resolved.

## Why it exists

Two inputs today name filesystem paths that Janus opens with its own
authority:

- **Hot file policy.** `POST /1.0/apps` accepts any clean absolute path as
  a `files.roots[].path`, `files.shell`, or `site.dir`
  (`validateConfiguredPath`, `files_config.go`). A control client that can
  register can serve `/home/<edge user>/.ssh` or Caddy's storage as a
  static root.
- **X-Sendfile.** A final upstream response may name any absolute path
  Janus can open (`applySendfile`, `sendfile.go`). The sendfile contract
  states the invariant "Janus has no broader filesystem authority than the
  application processes allowed to issue `X-Sendfile`" and leaves keeping
  it to the deployment.

The edge's own process holds the most sensitive files on the host: the
local CA's root key and every ACME account and certificate key under
Caddy's storage, the control token files, the access log (which records
URLs, and URLs can carry PHI), and other tenants' data. One compromised
upstream, one header-reflection bug in an application, or one control
client with a stolen or absent token reads all of it. A deployment
carrying PHI cannot rest that bound on an operator obligation nobody
checks. Rule 1 asks for a loud failure; this directive is how Janus fails
loudly instead of trusting.

## Scope and cascade

- Surface: one directive, `confine`, in the global `janus { }` block.
- Scope: **process-wide.** Hot registration arrives on the process-wide
  control plane, which does not choose a Caddy site (the files contract
  already says so), and the pooled data plane applies sendfile for every
  site. A per-site list would have no site to consult at registration.
- Cascades: **no.** A site-level `confine` is a parse error, exactly as
  `control` and `mdns` are.
- Applies to: `files.roots[].path` (including roots registered with
  `browse: true`, which is how hot browse and `janus serve` register),
  `files.shell`, `site.dir`, and every `X-Sendfile` target.
- Does not apply to: cold Caddyfile paths (cold `browse { root … }`,
  custom themes, renderers, `log` outputs, `tls` files). Cold config is the
  operator's own authority. The always-denied set below still applies to
  cold browse roots.

The capability numbering does not change. `confine` belongs to files and
sendfile; sendfile's "Configuration: none" becomes "Configuration: the
process-wide `confine` set, shared with files; Cascades: no".

## Caddyfile grammar

```caddyfile
{
	janus {
		confine /srv/medlabs /var/lib/redmine/files
	}
}
```

or, for a longer list, the block form:

```caddyfile
{
	janus {
		confine {
			/srv/medlabs
			/var/lib/redmine/files
			/Users/steve/Sites
		}
	}
}
```

| Element | Legal values | Default |
| --- | --- | --- |
| `confine <dir>…` | 1–64 absolute, clean Unix paths (the `validateConfiguredPath` rules: no NUL, backslash, `//`, `.`/`..` segment, or template) | — |
| block form | one path per line, same rules; arguments and a block together are an error | — |
| absent | nothing is confined-in: every hot root, shell, `site.dir`, and `X-Sendfile` target is refused | **deny** |

Every entry must exist and be a directory when the configuration
provisions. Entries are unique and never nest: `/srv` together with
`/srv/medlabs` rejects, because the inner entry is redundant and an
operator who narrows the outer one later would not notice the inner one
keeping a tree open.

There is no `off`, `none`, `any`, or `/`. Absence is the deny-everything
state, and `/` always contains a denied directory (below).

### The always-denied set

Provision computes a set of directories no entry may equal, sit inside,
or contain:

- Caddy's data directory (`caddy.AppDataDir()`) and config directory
  (`caddy.AppConfigDir()`);
- the root of the configured storage when it is `file_system` storage;
- under the service edge, Janus's config directory (the Caddyfile's
  directory, which holds `sites/` and `env`) and state directory (which
  holds `scope.json`, `run/`, and `log/`), published to the module by the
  binary the same way it publishes the exposure scope;
- the directory of every private key file the configuration names (`tls
  <cert> <key>`, control `key:`), and of every file `log` output.

"Contains" and "inside" are judged by identity, not spelling: provision
records the `(device, inode)` of each denied directory and of each of its
ancestors up to `/`, and the same for each entry. An entry is refused when
its identity appears among a denied directory's ancestors (it contains
one), or a denied directory's identity appears among the entry's ancestors
(it is inside one). Case-insensitive volumes, symlinked spellings, and
`..` cannot route around the check.

## Enforcement

Provision opens each entry once with `os.OpenRoot` and publishes the set
of handles, with each entry's configured spelling and its
symlink-resolved spelling, into pooled state at commit (the stage-and-
commit model hub and auth already use). The set a request uses is the one
committed when the request began; a retired set's handles close after the
last request holding it finishes (reference-counted).

A candidate path selects at most one entry: the entry whose configured or
resolved spelling equals the path or is a whole-segment prefix of it
(`/srv/med` never matches `/srv/medlabs/x`). Because entries never nest,
there is never a second candidate. The remainder is opened beneath that
entry's handle:

| Input | Today | Under `confine` |
| --- | --- | --- |
| hot root / browse root | `os.OpenRoot(abs)` per request | `entry.OpenRoot(rest)` per request |
| `files.shell` | `os.OpenRoot(dir(abs))` then the base name | `entry.OpenRoot(dir(rest))` then the base name, nonblocking |
| `site.dir` gate | `os.OpenRoot(dir)` then one direct-child stat | `entry.OpenRoot(rest)` then the same stat |
| `X-Sendfile` | `unix.Open(abs, O_RDONLY\|O_NONBLOCK\|O_CLOEXEC)` | `entry.OpenFile(rest, O_RDONLY\|O_NONBLOCK)` (`os.Root` adds close-on-exec) |

`os.Root` resolves symlinks and `..` component by component and refuses
any step that would leave the entry, including through a symlink swapped
in mid-open. A symlink inside an entry that points elsewhere inside the
same entry still works; one that points out is a miss (files) or a 502
(sendfile). `.` and `..` keep their ordinary meaning in an `X-Sendfile`
value, as the sendfile contract says, but cannot climb out of the entry.
Every existing descriptor rule (metadata and bytes from the same
descriptor, regular files only, FIFOs never block) is unchanged.

Registration validates early so the failure is loud at the right place:
`POST /1.0/apps` rejects a root, shell, or `site.dir` that lies under no
entry by spelling. A `{site}` template counts as one segment, so
`/srv/medlabs/sites/{site}/app` validates against `/srv/medlabs`. The
registration check exists for the error message; the request-time open is
the authority, because a path valid at registration can be re-pointed by a
symlink afterwards.

## Error text

Registration answers **400** with the existing JSON error shape:

```text
files.roots[1].path "/etc/ssl" is outside every confine directory (/srv/medlabs, /var/lib/redmine/files); add its tree to confine in the global janus block and reload, or register a path inside one
```

```text
files.shell "/srv/other/index.html" is outside every confine directory; confine is absent, so no hot file path is accepted (add confine to the global janus block)
```

A refused `X-Sendfile` answers **502** with `Cache-Control: no-store` and
the existing body; the instruction never leaks. The log line names the
app, never the path (a file name such as `smith-labs-2026.pdf` is PHI in
its own right):

```text
janus rejected upstream sendfile response  app=medlabs  reason="instruction path is outside every confine directory"
```

Parse and provision errors:

```text
confine is process-wide; set it in the global janus block only
confine: "srv/medlabs" must be an absolute Unix path
confine: "/srv/medlabs" is listed twice
confine: "/srv/medlabs" is inside "/srv"; list only the outer tree
confine: "/srv/medlabs" does not exist
confine: "/srv/medlabs/app.conf" is not a directory
confine: "/Users/steve" contains Caddy's data directory "/Users/steve/Library/Application Support/Caddy"; name a narrower tree
confine: "/Users/steve/.local/state/janus/log" is inside Janus's state directory; it is never servable
confine takes paths as arguments or in a block, not both
browse root "/Users/steve/.config/janus": inside Janus's config directory; it is never servable
```

## Reload and restart

| Event | Effect |
| --- | --- |
| Identical reload | No-op: handles reopen in the staged generation, nothing retires |
| Widening reload | New trees become admissible; existing registrations untouched |
| Narrowing reload | At commit (never at stage), every registration with a root, shell, or `site.dir` outside the new set is retired with reason `registration_unconfined` — the same effect as `DELETE`: claims released, hub torn down — and one warn line per app names the app and the field. Its next heartbeat answers 404, the tenant re-registers, and the registration answers 400 with the message above |
| Aborted reload | The staged set's handles close; nothing retires |
| Entry swapped on disk (a symlink or rename deploy of the entry itself) | The committed handle still names the old directory until the next reload re-opens it; name a stable parent, not a deploy symlink |
| Restart | Registry empties as always; tenants re-register against the configured set |

`X-Sendfile` is judged per response, so a narrowing takes effect for
sendfile at commit with no registry action.

Rejected alternative: keeping an out-of-bounds registration and letting
its requests miss. That turns a configuration change into a quiet 404 or
shell fallback, which rule 1 forbids.

## Interaction with existing capabilities

- **files**: validation and request-time opens as above; `files off`
  changes neither (the control plane stores policy regardless of the cold
  switch, and so validates it regardless).
- **browse**: hot browse roots are files roots and are confined. Cold
  browse roots are exempt from `confine` and checked against the
  always-denied set at provision.
- **`janus serve`**: registers a browse root over `/1.0`, so a directory
  outside `confine` is refused with the 400 above; `serve` prints the
  message and the line to add. See decision 3.
- **sendfile**: unchanged except the open (and the new 502 cause).
  Sendfile failures still never mark an upstream unhealthy.
- **access log**: a refused sendfile records class `sendfile` with status
  502, as any sendfile failure does.
- **control**: `GET /1.0` gains `"confine": ["/srv/medlabs", …]` (`[]`
  when absent). Paths are configuration, not request data.
- **metrics** ([draft](20261010-210100-metrics-and-alerting.md)):
  `janus_sendfile_rejected_total{reason="unconfined"}` and
  `janus_apps_retired_total{reason="unconfined"}`.

## Trust boundary, restated

`confine` bounds what Janus will open on behalf of hot input. It is a real
bound when the processes that register or issue `X-Sendfile` have less
filesystem authority than the edge: an upstream running as another user,
a control client reaching a TCP listener, or an application whose bug lets
a client steer a response header. An upstream running as the edge's own
user can already read every file the edge can; `confine` does not change
that, and the deployment guidance for PHI is separate users for the edge
and its applications.

Hard links are outside `os.Root`'s model: a hard link inside an entry to a
key file outside it is the same file. Linux's `fs.protected_hardlinks`
stops a user creating one to a file it does not own; on any host, keep the
confined trees and the denied directories on separate filesystems where
the threat model needs it. A bind mount or macOS firmlink of a denied
directory placed inside an entry is likewise the operator's to avoid.

## Migration

- **Seed** (`cmd/janus/service_paths.go` `seedConfig`): the global block
  gains a commented example and the reason, so a fresh install denies
  every hot path until the operator names a tree:

  ```caddyfile
  		# Directory trees apps may register as file roots and name in
  		# X-Sendfile. Absent, none: Janus opens nothing on an app's word.
  		# Caddy's storage and Janus's own config and state are never
  		# servable, whatever is listed here.
  		# confine /srv/apps
  ```

- **Existing installs** lack the directive, so the release that lands this
  refuses every hot root and `X-Sendfile` until the operator adds it. The
  installed binary is replaced before the running edge restarts, which
  gives a zero-refusal order: install the new release (the running edge
  keeps its old code), add `confine` naming the tenants' trees, `janus
  validate`, then `janus restart`. A `janus reload` before the restart
  fails loudly on the old code (`unrecognized janus subdirective:
  confine`) and changes nothing. The operator tutorial and CHANGELOG carry
  the procedure; `janus status` prints `confine: absent (hot files and
  X-Sendfile refused)` while the directive is missing.
- **Root `Caddyfile`, `Caddyfile.example`, `Caddyfile.minimal`**: the root
  file names the acceptance fixture's tree; the example shows both forms;
  the minimal file carries the commented line. All three still validate.

## Hard errors

Parse: site-level `confine`; repeated directive; no entries; more than 64;
arguments and a block together; a relative, unclean, or templated entry; a
duplicate.

Provision: an entry that does not exist, is not a directory, cannot be
opened as a root, nests with another entry, or equals, contains, or sits
inside a member of the always-denied set; a cold browse root that equals,
contains, or sits inside a member of the always-denied set.

Registration (400): a root, shell, or `site.dir` outside every entry, or
any of them while `confine` is absent.

Response (502): an `X-Sendfile` target outside every entry, one that
escapes its entry, or any target while `confine` is absent.

## Acceptance

Go tests (`go test ./...`):

- Parse: both legal forms; every parse error above with its message; a
  site-level occurrence.
- Provision: missing, non-directory, nested, and duplicate entries; the
  always-denied set with `XDG_DATA_HOME`/`XDG_CONFIG_HOME` pointed at
  fixture directories, a `file_system` storage root, a named `tls` key, and
  published service-edge directories; containment detected through a
  symlinked spelling and through a differently cased spelling on a
  case-insensitive volume (skipped where the test volume is
  case-sensitive); macOS `/var` → `/private/var` entries match both
  spellings.
- Registration: inside → 201; outside → 400 with the exact text, for
  roots, shell, and `site.dir`; a `{site}` root validated by its prefix;
  absent `confine` → 400 naming the directive; `/srv/med` versus
  `/srv/medlabs` segment boundary.
- Files at request time: a symlink inside an entry that points outside is
  a miss; one that stays inside serves; the existing symlink-swap race test
  re-run beneath an entry handle never escapes; FIFO roots still never
  block.
- Sendfile: inside → today's full matrix unchanged (re-run under an
  entry); outside, `..` escape, symlink escape, and absent `confine` → 502,
  no `X-Sendfile` leak, upstream not marked unhealthy, log line without the
  path, counter incremented.
- Lifecycle, driving stage and commit as the hub and auth reload tests do:
  narrowing retires at commit and not at stage, with the retired app's hub
  torn down; an aborted generation retires nothing and closes only its own
  handles; identical and widening reloads touch no registration; a retired
  set's handles close only after an in-flight request releases them.

`./test.sh`:

- `files` group: the root `Caddyfile` gains `confine` naming the fixture
  tree; a registration outside it answers 400 with the message; the
  existing cases still serve.
- `sendfile` group: an instruction naming a file outside the fixture tree
  answers 502 without the header; the existing delivery cases pass.
- `files` group, reload case: a reload onto a narrower `confine` removes
  the app from `/1.0/apps` and its next heartbeat answers 404.
- `browse` group: `janus serve` of a directory outside `confine` exits
  non-zero with the message.

## Measurement

The request path changes from one `os.OpenRoot(abs)` to
`entry.OpenRoot(rest)`, which walks the remainder component by component.
The five-run files matrix (first-root hit, second-root hit, shell
fallback, static miss) and the sendfile 1 MiB matrix re-run before and
after, interleaved, with roots three and six components below their entry;
raw provenance as `docs/*-bench-raw-confine.txt` and a ledger row. Expect
microseconds per request at most; a regression beyond the files rows'
run-to-run spread is reported, not absorbed. An entry-relative cache of
opened root handles is the remedy if one is needed, and is not built
without the number.

## Non-goals

- Per-app or per-site allowlists; hot `/1.0` edits of the list (rule 2).
- Confining cold Caddyfile paths beyond the always-denied set.
- Defending against an upstream that runs as the edge's own user.
- Hard-link, bind-mount, or firmlink detection.
- Mapping internal paths to URLs, or restricting sendfile to an app's own
  registered roots.

## Decisions for the owner

| # | Decision | Recommendation |
| --- | --- | --- |
| 1 | Absent `confine` | Deny everything from the landing release, with the install-edit-validate-restart order above. Alternative: one release where absent means unconfined with a warn line on every Start and in `janus status`, then deny |
| 2 | Narrowing reload | Retire out-of-bounds registrations at commit, loudly |
| 3 | `janus serve` outside `confine` | Refuse with the line to add; no exception for the `internal` socket. Alternative: the seed names a conventional tree (`~/Sites`) so `serve` works there out of the box |
| 4 | Always-denied set includes `log` output directories | Yes: access logs carry URLs |

## Adversarial review

Attacks on the draft above, as first written:

1. **Same-user upstream.** A compromised upstream running as the edge's
   user reads Caddy's storage directly; `confine` buys nothing there. The
   first draft implied otherwise.
2. **Tokenless `local` control listener.** `control local` without
   `token:` lets any local user, of any uid, register roots. `confine` is
   then the only bound, which is the strongest argument for it and also a
   reason to say plainly that a PHI host sets a token.
3. **Spelling games on the denied set.** The first draft compared
   symlink-resolved strings. APFS is case-insensitive by default:
   `confine /Users/steve/library` passes a case-sensitive string check
   against `/Users/steve/Library/Application Support/Caddy` and contains
   the CA's root key.
4. **Path in the log.** The first draft logged the refused sendfile path
   for the operator's benefit. File names carry PHI.
5. **Deploy symlink as an entry.** `confine /srv/app/current` with
   `current → releases/v42` pins v42 forever, because the handle is opened
   once per generation. A deploy flips the symlink and every request
   misses.
6. **Silent miss after narrowing.** Leaving registrations in place after a
   narrowing reload makes requests quietly fall to the shell or 404.
7. **Nested entries.** `/srv` plus `/srv/medlabs`: narrowing `/srv` away
   leaves `/srv/medlabs` open and the operator thinks it closed.
8. **`/` or `$HOME` as an entry.** Both contain the storage; an operator
   writing `confine /` to make an upgrade work defeats the capability.
9. **Hard links and bind mounts.** `os.Root` cannot see them.
10. **Registration-time check as the authority.** A path valid at
    `POST` can be re-pointed by a symlink later.
11. **Upgrade ordering.** The old binary rejects the new directive and
    the new binary denies everything without it; a naive procedure has a
    refusal window.
12. **Handle lifetime.** Closing a retired generation's entry handles
    while a request walks beneath them fails that request at random.

## Revisions

- Trust-boundary section added; states that `confine` bounds hot input
  only where the registrant or upstream has less authority, and points PHI
  deployments at separate users (1).
- `local` without a token named in the trust boundary (2); requiring a
  token there is left to the control contract.
- Denied-set containment judged by `(device, inode)` ancestry, not by
  spelling (3).
- Sendfile refusal logs the app and reason only (4).
- Entries open per generation, reopen at reload, and the reload table
  tells the operator to name a stable parent (5).
- Narrowing retires registrations at commit with reason
  `registration_unconfined` (6).
- Nested entries are a provision error (7).
- No `/`, `off`, or `any`; containing a denied directory is an error, so
  `/` and a home directory with Caddy's storage inside are refused (8).
- Hard links, bind mounts, and firmlinks documented as limits with the
  `fs.protected_hardlinks` and separate-filesystem guidance (9).
- Request-time `os.Root` open stated as the authority; registration
  validation exists for the message (10).
- Migration order made explicit: install, edit, validate, restart (11).
- Retired handle sets are reference-counted (12).
