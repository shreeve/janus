# Acceptance suite

Run `make test` from the repository root. `test.sh` owns one foreground
process, the tally, fixture lifetimes, cleanup, and capability order. These
files are sourced case groups, not standalone test programs. Do not run the
driver as a detached background job.

The suite creates a short `/tmp/janus-test.XXXXXX` directory to keep Unix
socket paths within platform limits. Writable fixtures, sockets, Caddy
storage/autosaves, logs, testkit, and temporary tenant/config files belong
there. Checked-in certificates and source remain read-only. A successful
run removes its directory; a failed run retains it and prints its location.
`KEEP_TEST_STATE=1` retains successful runs too. `CADDY_LOG` can explicitly
select another log path.

The real Rip tenant is required. CI pins its revision in
`.github/workflows/check.yml`; record the new revision and run the complete
suite when updating it. Local runs use the sibling `../rip` checkout unless
`RIP_ROOT` is set. Fixed integration ports must be free; the driver refuses
to terminate an unrelated listener.

Reload fixtures retain the running heartbeat TTL. A test that changes TTL
must assert rejection, or restart the edge to apply it. Keep all other
reload-survival assertions intact when adding a case.
