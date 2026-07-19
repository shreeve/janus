# certs/

**This directory is public.** Everything committed here is visible on GitHub.
Do **not** put private keys, customer certs, personal material, or any other
privacy-sensitive content in this folder — only the intentional `*.ripdev.io`
local-dev wildcard pair below.

## Why these two files are here

| File | Role |
| --- | --- |
| `ripdev.io.crt` | Certificate (SAN: `*.ripdev.io`, `ripdev.io`) |
| `ripdev.io.key` | Private key for that cert only |

They are **designed** to live in a public repo. The `ripdev.io` zone resolves
**only** to `127.0.0.1`. Use them for local HTTPS / SNI demos (no `curl -k`).
They are not for binding a public IP.

GitHub secret scanning ignores this directory via
[`.github/secret_scanning.yml`](../.github/secret_scanning.yml).
Other files under `certs/` are gitignored except this README and the two
files above.
