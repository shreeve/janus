# macOS pf runtime verification: a root verifier and a verdict the edge obeys

Status: **Draft — awaiting owner review.** Amends
[exposure modes](20260907-000352-exposure-modes.md), "What holds the
contract, per platform" (macOS). Nothing here is implemented.

## Why it exists

On macOS the edge's socket is the wildcard in every mode, and pf is what
holds `localhost` and `lan` to their addresses. The edge checks pf exactly
once, and not even then: at start it confirms the anchor *files* are in
place (`pfInstalled`, `cmd/janus/firewall_apply.go`), because only root can
ask pf what it is enforcing. After start, the exposure watch skips TCP on
darwin entirely (`verifyExposure`, `cmd/janus/listeners.go`: "TCP is not
judged at all"). So:

- `pfctl -d`, run by anything with root (an installer, a VPN client, a
  person debugging), disables pf, and the edge serves every interface;
- `pfctl -F all` flushes the main ruleset, including the `anchor "janus"`
  reference, and the anchor's rules, still loaded, are never evaluated;
- `pfctl -f <another file>` loads a ruleset without the reference, with
  the same result;
- a macOS update rewrites `/etc/pf.conf` without the janus lines. This
  happened on the owner's machine on 2026-10-05. An edge that finds the
  anchor unwired correctly refuses to serve, and the supervisor restarts it
  into the same refusal; an installed edge stayed down that way for five
  days and nobody knew (the
  [metrics draft](20261010-210100-metrics-and-alerting.md) covers the
  knowing).

The first three are silent widenings of a `localhost` or `lan` edge to
`wan` exposure, the exact failure the exposure contract exists to prevent.
Even root's own check (`checkPf`) misses the second and third: it compares
the anchor's loaded rules with the anchor file, but never asks whether the
main ruleset still references the anchor.

## Scope

- macOS only, `localhost` and `lan` modes only. Linux binds exact
  addresses and needs no verifier; `wan` needs no firewall; Windows has no
  exposure modes.
- No Caddyfile surface (cascades: **no**; nothing to cascade). The
  verifier is part of the binary's firewall step, installed by the one
  root step that exists today, `sudo janus firewall`.
- Two parties: a **root verifier** that can see pf, and the **user edge**
  that obeys its verdict.

## The root verifier

### What runs

The existing `janus.pf` LaunchDaemon, which today runs `/sbin/pfctl -E`
once at load, becomes the verifier:

```xml
<key>Label</key>             <string>janus.pf</string>
<key>ProgramArguments</key>
<array>
	<string>/Library/PrivilegedHelperTools/com.github.shreeve.janus.pf</string>
	<string>pf-verify</string>
</array>
<key>RunAtLoad</key>         <true/>
<key>KeepAlive</key>         <true/>
```

The program is a **root-owned copy** of the janus binary
(`root:wheel 0755` in `root:wheel 0755` `/Library/PrivilegedHelperTools`),
written by `sudo janus firewall` from the invoking binary, same signing
identity. Root never executes the user's `~/Applications/Janus.app`
binary: that file is the user's to replace, and a root daemon running it
would hand root to anything that can write it. `pf-verify` refuses to run
if its own executable, its directory, or the plist are writable by anyone
but root.

`pf-verify` reads only root-owned files: `/etc/pf.conf`,
`/etc/pf.anchors/janus`, and its own copy of the last anchor it installed,
`/Library/Application Support/Janus/pf.anchor`. It never reads the user's
`scope.json` or anything under a user's home (the exposure contract's
closed door: root acting on user-writable input is a confused deputy).
The anchor content therefore always comes from a `sudo janus firewall`
run, which already derives it from the user's scope through `SUDO_UID`.

### What it checks

Every two seconds, holding `/var/run/janus/pf.lock` (shared with `janus
firewall`, so an apply and a check never interleave):

| # | Check | Command |
| --- | --- | --- |
| 1 | pf is enabled | `pfctl -s info` reports `Status: Enabled` |
| 2 | the main ruleset references the anchor, ahead of Apple's | `pfctl -s rules` lists `anchor "janus" all` before `anchor "com.apple/*" all` |
| 3 | the anchor holds exactly the installed rules | `pfctl -a janus -s rules` equals `pfctl -a janus -nvf /etc/pf.anchors/janus` |
| 4 | the anchor file is the installed one | `/etc/pf.anchors/janus` equals the root copy byte for byte |
| 5 | `pf.conf` declares and loads the anchor | `pfWired(/etc/pf.conf)` |

Check 2 is the one today's `checkPf` lacks; `janus status` run as root
gains it too.

### What it does about a failure

| Failure | Repair | Class |
| --- | --- | --- |
| pf disabled (1) | `pfctl -E` | runtime |
| anchor rules differ or are gone (3) | `pfctl -a janus -f /etc/pf.anchors/janus` | runtime |
| anchor file differs or is gone (4) | restore it from the root copy, then (3) | file |
| main ruleset lost the reference (2), `pf.conf` still wired (5) | `pfctl -f /etc/pf.conf` | ruleset |
| `pf.conf` lost the janus lines (5) | re-insert them ahead of Apple's anchor with the same parse-check and rollback `janus firewall` uses (`pfConfWithAnchor`), then `pfctl -f /etc/pf.conf` | file |

Every repair is recorded (below) and logged to the system log under the
`janus.pf` label with what was found and what was done. A repair restores
only the state `sudo janus firewall` installed and recorded; it never
derives anything new.

**Repair budget.** Ruleset and file repairs are limited to three per
rolling hour. Another tool that owns pf (a VPN client, a container
runtime, a firewall product) and reloads its own ruleset would otherwise
fight the verifier forever, each stomping the other every two seconds.
Past the budget, the verifier stops repairing those classes and the
verdict is `failing` with the reason `another program keeps replacing
pf's ruleset (3 repairs in the last hour); the edge stays down until
'sudo janus firewall' runs again`. Runtime repairs (enable, anchor reload)
have no budget: they touch nothing another tool owns.

Repair is not tolerance (rule 1): the edge never serves while the verdict
is anything but `verified`, every repair is visible in `janus status`, and
the budget turns a recurring fight into a stop.

### The verdict

The verifier writes `/var/run/janus/pf.json` (`root:wheel 0644`, in
`root:wheel 0755` `/var/run/janus`, atomic rename) after every pass:

```json
{
  "verdict": "verified",
  "checked": "2026-10-10T08:01:03.412Z",
  "interval_ms": 2000,
  "anchor_sha256": "5c1f…e0",
  "checks": { "enabled": true, "referenced": true, "rules": true, "anchor_file": true, "conf": true },
  "repairs": [
    { "at": "2026-10-05T09:12:40Z", "class": "file", "what": "/etc/pf.conf lost the janus lines (an OS update rewrites it); restored them" }
  ],
  "repairs_last_hour": 0,
  "helper": "1.20.0"
}
```

`verdict` is `verified` or `failing` (with a `reason`). `repairs` keeps the
last ten. `/var/run` is emptied at boot, so a verdict never survives a
restart of the machine; only a running verifier produces one. No user can
write the file or its directory.

## The user edge obeys

The exposure watch, every two seconds in `localhost` and `lan` on macOS,
reads `/var/run/janus/pf.json` and requires all of:

- `verdict` is `verified`;
- `checked` is within three intervals of now (the verifier is alive);
- `anchor_sha256` equals the SHA-256 of `PfAnchor(scope)` for the scope the
  edge reads from its own `scope.json` (the rules root enforces are the
  rules this mode needs).

A miss on two consecutive ticks is a violation, handled as every exposure
violation is: logged with the reason, the process exits with failure, the
supervisor restarts it, and the pre-start check refuses with the same
reason until the verdict is good again. Start refuses outright when the
file is absent: `the pf verifier is not running (run: sudo janus
firewall)`.

The start-refusal record of the metrics draft carries the reason, so
`janus status --check` reports it as CRITICAL. Non-root `janus status`
prints the verdict with its source instead of `unverified`:

```text
firewall verified by janus.pf at 08:01:03 (repaired 2026-10-05 09:12: /etc/pf.conf lost the janus lines)
```

A mode change does not trip the watch: `janus mode` runs `sudo janus
firewall`, which applies the new anchor, updates the root copy, and writes
a fresh verdict under the lock before it returns, so the edge's next tick
sees the new hash.

The residual window: between a change to pf and the edge stopping, at
most one verifier interval plus two watch ticks, about six seconds. pf
offers no change notification; the window is stated, not hidden.

## After a macOS update

The recorded case, 2026-10-05: the update rewrote `/etc/pf.conf` without
the janus lines. Which other files that update touched is not recorded;
the manual record below captures the next one. Under this contract, with
the anchor file and the daemon left in place, at the first boot after the
update Apple's job loads the new
`pf.conf`, the verifier starts at load, finds check 5 failing, re-inserts
the janus lines, reloads, enables pf, records the repair, and writes
`verified`; the user edge starts at login and serves. The repair shows in
`janus status` and the system log. Should an update also remove the anchor
file, the root copy restores it. Should one remove the helper or the
daemon, the edge refuses with "the pf verifier is not running", and the
not-running signal reports it.

## Interaction with existing capabilities

- **exposure modes**: the macOS row of "what holds the contract" gains
  the verifier; "Never overclaim" holds, since `verified` now comes from a
  root process and names it.
- **webtransport**: UDP 443 is in the same anchor; nothing changes.
- **mdns**: unaffected (multicast is not a front-door port).
- **`janus firewall --remove`** (scope `wan`, or uninstall): unloads the
  daemon, removes the helper, the plist, the root anchor copy, and
  `/var/run/janus`.
- **metrics**: `janus_firewall_verified` reads the verdict; repairs add
  `janus_firewall_repairs_total{class}`.

## Reload and restart

`janus reload` touches nothing here: the verdict is read by the binary's
watch, not the module. A restart of the edge re-reads the verdict at
pre-start. A reboot empties `/var/run`; the daemon runs at load and writes
the first verdict before any user logs in.

## Migration

No Caddyfile or seed change. An existing macOS edge in `localhost` or
`lan`, upgraded, refuses to start until `sudo janus firewall` installs the
helper, the daemon change, and the root anchor copy; the refusal names
that command. This is the same posture webtransport took for an anchor
written before UDP 443 joined it (that contract's decision 4). A `janus
autostart` on a fresh Mac already runs `sudo janus firewall`, so new
installs get the verifier with no extra step. The helper is refreshed by
every `sudo janus firewall`; `janus status` warns when the helper's
version differs from the binary's, and the verdict format is versioned so
an older helper keeps working until then.

## Hard errors

- Edge pre-start (macOS, `localhost`/`lan`): verdict absent, stale,
  failing, or for another anchor.
- Edge watch: the same, twice in a row.
- `pf-verify`: its executable, directory, or plist writable by a non-root
  user; it logs and exits non-zero (launchd throttles it, and the edge,
  seeing no fresh verdict, stops).
- `janus firewall`: a helper copy that cannot be written root-owned.

## Acceptance

Go tests (`cmd/janus`, every pf and file action through `fwOps` fakes, so
no test touches the host's pf):

- Each check row detected individually, including check 2 with the anchor
  rules still loaded after a simulated `-F all` (today's false
  `verified`).
- Each repair row performed, recorded, and logged; the parse-check
  rollback when a re-inserted `pf.conf` fails to parse.
- The repair budget: the fourth ruleset repair in an hour is not
  performed and the verdict turns `failing` with the reason; runtime
  repairs continue.
- Verdict file shape, atomic write, `repairs` bounded at ten.
- Self-check: a helper or plist writable by group or other refuses.
- The edge: verified accepted; failing, stale, absent, and wrong-hash
  verdicts each refused at pre-start and stopped after two ticks; one bad
  tick tolerated; a mode change with the verdict written by `janus
  firewall` never trips.
- `janus status` lines for each verdict, as root and not.
- `janus firewall` installs and `--remove` removes every file named here.

`./test.sh`: a `pf` group that runs only when `JANUS_ACCEPT_PF=1` is set,
which `check.yml` sets on its disposable macos-15 runner (already under
sudo); elsewhere it prints one `SKIP pf (set JANUS_ACCEPT_PF=1 on a
disposable Mac)` line. On the runner it installs the verifier, starts a
`lan`-mode edge, then in turn runs `pfctl -d`, `pfctl -F all`, and loads
a ruleset without the anchor, asserting each time that the verdict
records a repair within five seconds and the edge keeps serving; then it
loads a foreign ruleset four times within the hour and asserts the edge
stops with the budget reason.

A manual record on the owner's machine (the repairs and their timings for
each tamper, and the post-update boot) is added to this document before it
is ruled final, as the webtransport browser proof was.

## Measurement

Performance-neutral for requests: the watch already reads files every two
seconds. The verifier's cost (five `pfctl` invocations every two seconds)
is measured once as CPU per hour on the owner's machine and recorded in
the manual record.

## Non-goals

- Linux, Windows, or `wan`.
- Coexisting with another pf owner beyond detecting the fight and stopping.
- Root binding exact addresses and passing descriptors to the user edge
  (launchd `Sockets` in a daemon with `UserName`); that would replace pf
  scoping altogether and is its own contract (see review item 9).
- Notifying a person; the metrics draft owns the not-running signal.

## Decisions for the owner

| # | Decision | Recommendation |
| --- | --- | --- |
| 1 | Repair or only report | Repair, recorded, within the budget; alternative: report only and keep the edge down until a person runs `sudo janus firewall` |
| 2 | Verifier implementation | Root-owned copy of the janus binary; alternative: a root-owned shell script calling `pfctl` (smaller, but re-implements the `pf.conf` edit in shell) |
| 3 | Interval and window | 2 s verifier, two 2 s ticks at the edge |
| 4 | Existing installs | Refuse until `sudo janus firewall` installs the verifier |
| 5 | Repair budget | Three ruleset or file repairs per rolling hour |

## Adversarial review

1. **Root runs the user's binary.** The first draft pointed the daemon at
   the installed `janus` command, which on macOS lives in
   `~/Applications/Janus.app`. Any process running as the user replaces it
   and gets root at the next interval.
2. **Root reads user input.** Having the verifier derive the anchor from
   the user's `scope.json` reopens the confused deputy the exposure
   contract closed (a symlinked `scope.json`).
3. **Forged verdict.** A verdict file in the user's state directory, or in
   a world-writable directory, is forgeable by any process as the user,
   which then serves wide with a green status.
4. **Stale verdict.** A verifier that crashed or was unloaded leaves a
   `verified` file that stays green forever.
5. **Verdict for the wrong mode.** After `janus mode lan`, a verdict for
   the old `localhost` anchor is still `verified`.
6. **The reference check is missing.** `pfctl -F all` leaves anchor rules
   loaded and unreferenced; the first draft, like today's `checkPf`,
   called that verified.
7. **Fighting another pf owner.** Reloading `pf.conf` every two seconds
   against a VPN client that reloads its own ruleset breaks the VPN and
   never converges.
8. **Silent repair.** Restoring `pf.conf` after an update with no record
   is exactly what rule 1 forbids, and hides a recurring cause.
9. **The whole approach.** A root process holding the sockets (launchd
   `Sockets` bound to exact addresses, handed to the edge running as the
   user) would make pf unnecessary. Tempting; it changes how every mode
   binds and how `lan` follows a DHCP move.
10. **The window.** No polling design closes it.
11. **Apply races check.** `janus firewall` loading a new anchor while the
    verifier compares rules sees a mismatch and "repairs" back to the old
    file.

## Revisions

- The daemon runs a root-owned copy in `/Library/PrivilegedHelperTools`
  that checks its own ownership (1).
- The verifier reads only root-owned files; the anchor comes from `sudo
  janus firewall` and is kept as a root copy (2).
- The verdict lives in root-owned `/var/run/janus` (3).
- Freshness bound of three intervals; `/var/run` empties at boot (4).
- The verdict carries the anchor's hash, checked against the edge's own
  scope (5).
- Check 2 added, and to root's `janus status` (6).
- The repair budget, and a stop with a named reason past it (7).
- Every repair recorded in the verdict, the system log, and `janus
  status` (8).
- Recorded as a non-goal for a separate contract (9).
- The window is stated in the contract (10).
- One lock shared by `janus firewall` and the verifier; `janus firewall`
  writes the verdict before returning (11).
