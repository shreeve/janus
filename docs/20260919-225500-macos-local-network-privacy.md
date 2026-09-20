# macOS Local Network privacy and the janus edge

Record of the 2026-09-19 investigation on a freshly installed macOS 27.0
Mac (Apple Silicon, Wi-Fi `en0` at 10.0.0.27): the installed janus edge
announced `janus.local` and nothing on the LAN, this Mac included, could
resolve it. This page states what the problem was, what fixed it, what
the repository changed so it does not recur, and the one-pass procedure
for a new Mac. The living procedure is in the
[operator contract](operators/index.md#macos-local-network-privacy); the
capability page carries the caveat row.

## 1. What the problem was

**Symptom.** `janus status` healthy, `/1.0/mdns` reporting `janus.local`
announced, TCP on ports 80 and 443 answering by address. Yet a phone's
Safari said the server stopped responding, `dns-sd -G v4 janus.local` on
the Mac returned nothing, and `dns-sd -B _http._tcp` showed the `janus`
instance on interface 1 (loopback) only. A `tcpdump -i en0 udp port 5353`
showed the phone asking for `janus.local` every few seconds and no reply
ever leaving 10.0.0.27, while the Mac's own Bonjour daemon answered other
questions from the same address. No line in the janus log.

**Cause.** macOS 15 introduced Local Network privacy. It exempts launchd
daemons, processes running as root, and tools run from Terminal (with
their children). It does not exempt a user launchd agent, which is what
`janus autostart` installs. Receiving an inbound UDP multicast counts as
local network access, and the check runs per socket inside the kernel's
UDP input path: the datagram is visible to tcpdump and delivered to
mDNSResponder, but silently not delivered to the denied process, with no
counter and no error. On the fresh install, the first launch of the edge
under launchd recorded a deny rule for the executable. The rule was filed
under the executable's code-signing identifier, which for a Go binary
that nobody named is `a.out`, so the row in System Settings did not say
"janus" and was not found for hours.

**Where the decision lives.** Not in TCC. The rules are in
`/Library/Preferences/com.apple.networkextension.plist` (world-readable
with `plutil -p`), keyed by signing identifier plus path, and matched
through a cache of executable build UUIDs. `tccutil reset LocalNetwork`
fails, disabling a row does not remove it, and `sudo rm` of the file is
refused; only Recovery mode can delete it, and Apple offers no supported
reset.

**Why it took an evening.** The failure has no error surface. Every
plausible layer was ruled out first with live evidence: pf (TCP only),
the application firewall (off), IPv6 (not needed), the CA (trusted), the
dnssd library (a minimal Go responder using it answered instantly), the
launchd environment (irrelevant), and the Go toolchain (both 1.25.1 and
1.27.1 builds failed). The decisive experiments were a byte-identical
copy of the binary at another path (equally deaf), the same bytes with a
new code hash (answering within seconds), and finally the rule table in
the plist.

**Two traps met on the way out.**

- *Renaming an installed binary.* Re-signing `~/.local/bin/janus` under
  successive identifiers left four rules sharing one build UUID. Under the
  last name macOS created no rule at all and the edge stayed deaf under
  every name. A new name needs a freshly built binary.
- *Overwriting an executable in place.* `cp` onto the existing file keeps
  the inode whose signature macOS has cached. Every later launch of it is
  killed (exit 137), and an edge already started from it has no identity
  macOS can match, so no rule is ever created. Replace a binary by writing
  a new file and renaming it into place, as `install.sh` does.

Verified working state at the end: a fresh v1.17.0 build signed
`com.github.shreeve.janus`, installed through a new file, its Local
Network row on, one `janus restart` after the row existed. `janus.local`
then answered a live multicast query on `en0` within a second, Bonjour
resolved it on the Wi-Fi interface, and the phone loaded the trust page.

## 2. What the repository changed

- `Makefile`: `make janus` signs `bin/janus` as `com.github.shreeve.janus`
  on macOS.
- `.github/workflows/release.yml`: the macOS build is signed before
  packaging, and the smoke test asserts the identifier.
- `scripts/release-install.sh` (the archive installer and `make install`):
  signs the staged copy, a fresh file, when its identifier differs.
- `install.sh`: signs the extracted binary before handing off when an
  older archive's installer predates this.
- `install_test.go`: pins that an `a.out` binary is signed on install and
  a correctly named one is left alone.
- README, HANDOFF, the operator contract, and the mdns contract describe
  the setting, the identity rule, and the two traps.

The identifier is `com.github.shreeve.janus`, reverse-DNS of the module
path. It is a label, not a credential: the signature stays ad hoc. Its
job is to be the same on every build forever, so one row in the Local
Network list follows the edge across upgrades.

## 3. One pass on a new Mac

1. Install janus with `install.sh` (or `make install`). Never `cp` over an
   installed binary; the installers write a new file and rename it.
2. `janus autostart`, then `janus mode lan` if phones will use it (this
   asks for `sudo janus firewall` once).
3. System Settings → Privacy & Security → Local Network: turn on the
   `janus` row (named `com.github.shreeve.janus` until a release embeds a
   display name). If the row is not there yet, wait a few seconds after
   the edge starts and reopen the pane.
4. `janus restart`. Required: a process keeps the decision it received
   when it started.
5. `janus trust` so this Mac trusts the edge's CA.
6. Each phone: `http://janus.local/trust`, then the three iOS steps it
   describes: download the profile, install it under General → VPN &
   Device Management, enable it under General → About → Certificate Trust
   Settings.

Confirm from the Mac: `dns-sd -B _http._tcp` shows `janus` on the Wi-Fi
interface index, not only on 1, and `curl -s -o /dev/null -w '%{http_code}'
http://janus.local/` prints 200. Bonjour caches answers for 120 s, so
after a change give it two minutes or query a name it has not seen.

If an allowed edge still hears nothing after a reboot, that is a known
macOS defect: turn the row off and on, then `janus restart`.

Resetting the list is possible only from Recovery (Apple offers no
supported reset). Shut down, hold the power button to the startup
options, Options → Continue, sign in; in Utilities → Terminal, a FileVault
volume must be unlocked and mounted first (`diskutil apfs list`, then
`diskutil apfs unlockVolume <id>`); then remove
`/Volumes/Macintosh HD/Library/Preferences/com.apple.networkextension*.plist`
and restart. Every application asks again, and VPN configurations in that
file are reset. Done twice on this Mac; the files regenerate at boot.

## 4. Follow-ups not done here

- Done the same night as an application bundle, `Janus.app`:
  [macos-app-bundle](20260920-001500-macos-app-bundle.md).
- Teach `janus status` to read the policy plist and report "denied by
  Local Network privacy" for its own path instead of nothing.
- Upstream to brutella/dnssd: no interface re-join on darwin after start;
  IPv6 sends to `ff02::fb` lack an interface zone and fail with "no route
  to host".

## References

- Apple TN3179, Understanding local network privacy:
  https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy
- Apple Developer Forums, multicast inconsistent on macOS 15/26:
  https://developer.apple.com/forums/thread/809211
- Apple Developer Forums, launch agent local network failure:
  https://developer.apple.com/forums/thread/778457
- XNU `udp_input` per-socket NECP check:
  https://github.com/apple-oss-distributions/xnu/blob/main/bsd/netinet/udp_usrreq.c
- Eclectic Light, Local Network privacy revealed:
  https://eclecticlight.co/2026/01/18/last-week-on-my-mac-local-network-privacy-revealed/
