# TODO — open design / correctness notes

Real-but-deferrable items. When one becomes real work, it gets a
contract doc first (`docs/YYYYMMDD-HHMMSS-*.md`). Remove items when
fixed or promoted into real docs/tests — git history is the record of
completed work.

---

## Open design

- [ ] **No scoping on host claims or socket paths within the trust
      domain.** Any authenticated control client can claim any host
      (first-wins, `apps.go` create/PATCH) and publish any socket path;
      hub publish and snapshot add cross-tenant reach beyond registry
      CRUD. This is the multi-tenant host-claim-scoping design the hub
      contract explicitly defers: see
      `docs/20260720-162350-hub-design.md` — "Multi-tenancy: the
      per-app namespace" (when scoping lands, publish and snapshot each
      require an explicit caller-owns-app authorization check; app-id
      routing alone is not authorization) and "The publish plane"
      (auth posture is exactly `/1.0`; per-app publish secrets stay a
      non-goal until a use case demands them). Until then, every token
      a control listener accepts is trusted across all apps visible on
      that listener.
