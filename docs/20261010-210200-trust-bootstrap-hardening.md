# Trust bootstrap hardening: a name-constrained local CA and an out-of-band compare

Status: **Draft — awaiting owner review.** Amends
[capability 4, mdns](20260722-034619-capability-mdns.md), "Owner ruling:
trust rides the front door (revision 7)". Nothing here is implemented.
Assumes the SHA-256 root fingerprint on the trust page, in `janus status`,
and in `janus trust --export`, which lands separately.

## Why it exists

`/trust` hands any device on the LAN the edge's local root, over plain
HTTP, as a certificate and as an iOS profile whose payload is
`com.apple.security.root` (`trustProfile`, `mdns_trust.go`). That is the
design: a device that trusts nothing yet cannot load anything over HTTPS,
so trust begins in the clear. Two consequences follow.

1. **The root is unconstrained.** Caddy's internal CA root
   (`caddypki`, `DefaultCAID`) carries no name constraints. A phone that
   trusts it accepts a certificate from it for any name: a bank, a mail
   provider, the PHI application's public host. The root's private key
   lives online in Caddy's storage on the edge, because Caddy re-signs its
   intermediate weekly. Anyone who reads that key (a backup, a stolen
   laptop, or the `X-Sendfile` path this repository's
   [confine draft](20261010-210000-files-sendfile-confine.md) closes) can
   intercept every TLS connection of every device that ever trusted it.
2. **The bootstrap is interceptable.** A LAN attacker answering
   `janus.local` (mDNS is unauthenticated; the mdns contract says so) or
   sitting on the path serves its own root on `/trust`. The device installs
   it and the attacker owns that device's TLS.

Name constraints address the first. Only a comparison over a channel the
attacker does not control addresses the second. This contract recommends
both, with the constrained root as the change and the comparison as its
bootstrap step.

## Options considered

| Option | Bounds a stolen root key | Detects a substituted root at bootstrap | Migration cost |
| --- | --- | --- | --- |
| **A. Janus-generated name-constrained root**, handed to Caddy through the stock `pki` app | Yes, to the permitted names, on every client that enforces anchor constraints | No | Every device re-trusts once |
| B. Fingerprint shown on `/trust`, in `janus status`, and in the platform's certificate view; the person compares | No | Yes, when the person compares | None |
| C. TOFU in the page (the HTTPS dashboard re-displays the root it holds) | No | No: an attacker whose root the device trusts serves the dashboard too and displays whatever matches | None |
| D. Name-constrained intermediate under Caddy's existing root | No: devices trust the unconstrained root, whose key sits beside the intermediate's | No | None |
| E. Status quo | No | No | None |

**Recommendation: A, with B as its bootstrap step.** A is the only option
that changes what a leaked key is worth, and the leak paths are real. B
costs nothing and is the only defense against substitution, but it depends
on a person doing it, so it cannot be the whole answer. C is circular and
D constrains the wrong certificate; both are rejected.

## The constrained root

### Generation

A new verb, `janus ca new`, generates an ECDSA P-256 root with a ten-year
lifetime (Caddy's own root lifetime) and writes it to the state directory:

```text
<state>/ca/root.crt   0644
<state>/ca/root.key   0600
```

The state directory is in the always-denied set of the confine draft, so
neither file is ever servable. The certificate carries:

| Field | Value |
| --- | --- |
| Basic constraints | CA, path length 1 (Caddy's intermediate, then leaves) |
| Key usage | certificate sign, CRL sign |
| Name constraints | **critical** |
| Permitted DNS | `local`, `localhost`, and every name the service Caddyfile serves with the internal issuer at generation time, plus each `--permit <name>` |
| Permitted IP ranges | `127.0.0.0/8`, `::1/128`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7` |
| Permitted email, URI | the sentinel `invalid` (an empty permitted set) |
| Subject CN | `Janus local CA (<host short name>) <year>` |

A DNS constraint without a leading dot permits the name and every name
beneath it, so `local` covers `janus.local` and `rip.local`. `--permit`
takes a DNS name (a public suffix is refused: `--permit com` is an
error) and is repeatable. The names collected from the Caddyfile are
printed before the root is written, so the operator sees exactly what the
root may vouch for.

### Handing it to Caddy

Caddy owns the CA at runtime; Janus only supplies the root through the
stock global `pki` option, which rule 3 allows because it is Caddy's own
grammar:

```caddyfile
{
	pki {
		ca local {
			root {
				cert /Users/steve/.local/state/janus/ca/root.crt
				key  /Users/steve/.local/state/janus/ca/root.key
			}
		}
	}
}
```

Caddy then generates and renews its intermediate under that root as it
does today. Constraints in the root bind the whole chain. `janus trust`,
`janus trust --export`, `/trust/ca.crt`, and `/trust/ca.mobileconfig` keep
reading the root from the pki app, so they serve the constrained root with
no change.

### What the module enforces

At Start, when the default CA's root carries name constraints:

- a site whose certificate comes from the internal issuer with a name
  outside the permitted set is a **Start error**: `site corp.example uses
  the internal issuer, but the local CA's root permits only local,
  localhost, rip.localhost; run 'janus ca new --permit corp.example' (every
  device re-trusts) or give the site a public certificate`;
- the on-demand permission (`permission janus`) refuses a name outside the
  permitted set, so Caddy never mints a certificate clients will reject.

When the root carries none, nothing is refused; `janus status` prints
`ca: unconstrained root (janus ca new replaces it; every device
re-trusts)` and `status --check` (metrics draft) reports WARNING.

## The bootstrap compare

The trust page, before the install step, shows the root's SHA-256
fingerprint in eight groups of four hex digits and one instruction:

```text
Before you trust it, compare this fingerprint with the one shown by
'janus status' on the edge's own machine:

  3F2A 91C0 7B1E 44D2 … 08AA 6E13

After installing, the same fingerprint appears under the profile's
certificate details. If any group differs, remove the profile: someone on
this network is answering for the edge.
```

With a constrained root the page also says what the certificate can do:
"It can vouch only for names ending in .local and .localhost (and the
names listed)." That sentence is generated from the root's own
constraints; an unconstrained root gets none.

The compare is advisory: nothing on a phone can enforce it, and the page
itself is what an attacker would rewrite. Its value is that the reference
copy, `janus status` on the edge's machine, is out of the attacker's reach.

## Plain HTTP and the HTTPS-only TODO

This contract preserves the plain-HTTP bootstrap unchanged. It adds one
rule the HTTPS-only work in `TODO.md` must keep: **the front door's names,
and every name served by the internal issuer, never carry
`Strict-Transport-Security`.** A device that remembered HSTS for
`janus.local` and then met a rotated root could reach neither
`https://janus.local` (untrusted) nor `http://janus.local/trust`
(upgraded by HSTS), and would be locked out of the one page that fixes it.

## Rotation and migration

| Install | Behavior |
| --- | --- |
| New (`janus autostart` writes the seed) | When `autostart` writes the seed and `<state>/ca/root.crt` is absent, it runs `janus ca new` before the first start; the seed carries the `pki` block above |
| Existing, unchanged | Caddy's unconstrained root stays; nothing breaks; status warns |
| Existing, migrating | `janus ca new`, add the printed `pki` lines to the Caddyfile, `janus validate`, `janus restart` |
| Adding a permitted name later | `janus ca new --permit <name>` and the same steps; a root's constraints cannot be edited |

A restart rather than a reload, because certificates already in Caddy's
cache chain to the old root; the restart drops the cache, and the internal
issuer's short-lived leaves re-issue under the new chain at the next
handshake. `janus ca new` refuses to overwrite an existing root without
`--replace`, and with it keeps the previous pair as `root.crt.prev`/
`root.key.prev` until the next replace.

Each device re-trusts once:

- **iOS**: the profile's `PayloadIdentifier` (`janus.edge.trust`) is
  unchanged, so installing the new profile replaces the old one and its
  root; the person turns the new root on in Certificate Trust Settings.
- **macOS**: `janus trust` installs the new root; `janus trust
  --remove-prev` removes the previous one from the keychain.
- **Android, other**: install `/trust/ca.crt`, remove the old certificate
  by hand.

The owner's own devices are a handful; the cost is one install per device,
once.

## Interaction with existing capabilities

- **mdns**: `/trust` routes, their publicity, and the plain-HTTP door are
  unchanged; the page gains the fingerprint, the compare instruction, and
  the scope sentence.
- **tls ask / on-demand permission**: also refuses names outside a
  constrained root.
- **webtransport**: the descriptor pins the leaf with
  `serverCertificateHashes`, which bypasses CA trust; unchanged.
- **auth**: the `/trust` exemption from site gates is unchanged.
- **exposure `wan`**: local names are refused; a constrained root changes
  nothing there.

## Reload and restart

The root is configuration Caddy reads at provision; a reload that changes
the `pki` paths reloads the CA. The Start check runs per generation. No
pooled Janus state is involved.

## Hard errors

- `janus ca new`: an existing root without `--replace`; `--permit` with
  an IP, a wildcard, a public suffix, or an invalid name; a state
  directory that is not the edge owner's.
- Start: an internal-issuer site outside a constrained root's permitted
  names (message above).

## Acceptance

Go tests:

- `janus ca new`: the certificate's constraints are critical and exact
  (DNS, IP, email and URI sentinels, path length 1); names collected from
  a fixture Caddyfile's internal-issuer sites; `--permit` validation;
  refusal to overwrite; `.prev` retention.
- Go `x509.Verify` against the constrained root: a leaf for `foo.local`,
  `a.b.localhost`, and `127.0.0.1` verifies; a leaf for `example.com`
  signed with the same root key, and one for `8.8.8.8`, fail.
- Caddy's pki app provisioned with the supplied root issues an
  intermediate and a leaf for a `.local` name.
- The Start error for an out-of-set internal-issuer site; the on-demand
  permission's refusal; no refusal with an unconstrained root.
- `/trust`: the fingerprint groups match the root's SHA-256; the scope
  sentence appears for a constrained root and not otherwise; the profile's
  identifier is unchanged across two different roots.
- `janus status` and `--check` lines for constrained and unconstrained
  roots.

`./test.sh`, `tls` group: the testkit gains a `ca` subcommand that writes
a constrained root; the root `Caddyfile` carries the `pki` block pointing
at it; a `.localhost` handshake verifies against it with `curl --cacert`;
the testkit's Go client rejects a leaf for an out-of-set name signed by
the same key (curl's TLS library differs per platform and is not the
oracle).

Platform enforcement is decided by test, as webtransport's CT finding was:
a leaf for an out-of-set name signed by an installed constrained root is
presented to iOS Safari, macOS Safari and Chrome, Android Chrome, Firefox,
and Windows Edge, and each result is recorded in this document before the
contract is ruled final. A platform that ignores anchor constraints is no
worse off than today.

## Measurement

None: no request-path change.

## Non-goals

- Running a CA outside Caddy's pki app, or an ACME server for LAN clients.
- Revocation (CRL, OCSP) for the local CA.
- Enforcing the compare, or certificate pinning on devices.
- Replacing Caddy's root automatically on existing installs.

## Decisions for the owner

| # | Decision | Recommendation |
| --- | --- | --- |
| 1 | Option | A (constrained root) with B (fingerprint compare) as its bootstrap step |
| 2 | Private IP ranges in the constraint | Yes: loopback, RFC 1918, ULA |
| 3 | New installs | `autostart` generates the constrained root; existing installs migrate by hand |
| 4 | HSTS on front-door and internal-issuer names | Never; the HTTPS-only TODO inherits the rule |
| 5 | `status --check` on an unconstrained root | WARNING |

## Adversarial review

1. **Constraints the wrong way round.** The first draft constrained an
   intermediate Janus would feed Caddy. Devices trust the root, and the
   root key sits in the same storage; a thief signs with the root.
2. **Permitted DNS only.** With only DNS subtrees set, IP addresses,
   email, and URIs are unconstrained (RFC 5280 constrains per name form).
   A stolen key mints a certificate for a raw public IP.
3. **Names outside the set fail late.** A site added later with `tls
   internal` for a new name gets certificates every constrained client
   rejects, discovered by a person staring at a browser error.
4. **HSTS lockout.** The HTTPS-only TODO, done naively, sends HSTS on
   `janus.local`; after a rotation the device cannot reach `/trust` by
   either scheme.
5. **Rotation leaves stale leaves.** A reload keeps cached leaves chained
   to the old root; devices that installed only the new root fail until
   each leaf renews.
6. **The page lies.** On a hostile LAN the page, its fingerprint, and its
   scope sentence are the attacker's; the compare is worthless if its
   reference copy comes from the same page.
7. **Two roots trusted forever.** Rotation without removal leaves the old
   unconstrained root trusted on every device.
8. **`--permit com`.** One flag widens the root to a whole public suffix.
9. **Enforcement is assumed.** Not every platform has historically
   enforced constraints on trust anchors.
10. **A new root on autostart for existing installs** would silently
    break every device that trusted the old one.

## Revisions

- The constrained certificate is the root, generated by Janus and handed
  to Caddy through the stock `pki` option; option D rejected (1).
- IP ranges are constrained to loopback and private space; email and URI
  get an empty permitted set (2).
- Start error and on-demand refusal for names outside a constrained root
  (3).
- The no-HSTS rule for front-door and internal-issuer names (4).
- Migration uses `janus restart`, and says why (5).
- The compare's reference copy is `janus status` on the edge's machine;
  the page says so (6).
- iOS replaces the profile by identifier; `janus trust --remove-prev` on
  macOS; Android by hand, stated (7).
- `--permit` refuses public suffixes, IPs, and wildcards (8).
- Platform enforcement is decided by a recorded test matrix before the
  contract is final (9).
- `autostart` generates a root only when none exists; existing installs
  migrate by hand (10).
