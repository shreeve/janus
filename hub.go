package janus

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
)

// Hub state and executor
// (docs/20260720-162350-hub-design.md "Membership model", "Delivery
// semantics", "The three planes").
//
// Hub state is per app, memory-only, and Janus-owned: conns and channels
// mirror each other under the app hub's lock, a channel whose member set
// empties is deleted, and every operation begins by resolving its app.
// The hub set lives in the pooled process state (caddy.UsagePool), so a
// Caddy config reload never tears a hub down; only registry DELETE and
// TTL reap do.

// hubConnIDLen: 16 chars of [a-z0-9] (~82 bits) — the id doubles as an
// unguessable address, minted by Janus, never chosen by a client.
const hubConnIDLen = 16

func mintHubConnID() (string, error) {
	b := make([]byte, hubConnIDLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = idSuffixAlphabet[int(b[i])%len(idSuffixAlphabet)]
	}
	return string(b), nil
}

// hubCounters is the always-on counter block, kept per app hub and summed
// process-wide at read time (atomics; a snapshot never blocks the hot path).
type hubCounters struct {
	framesIn       atomic.Int64
	deliveries     atomic.Int64
	publishes      atomic.Int64
	rejectedFrames atomic.Int64
	unknownTargets atomic.Int64
	slowCloses     atomic.Int64
	pingCloses     atomic.Int64
	bridgeSent     atomic.Int64
	bridgeFailed   atomic.Int64
	bridgeDropped  atomic.Int64
	bridgeGarbage  atomic.Int64
}

// hubSet is the process-wide map of app id → hub, in the pooled state.
type hubSet struct {
	mu   sync.Mutex
	apps map[string]*appHub
}

func newHubSet() *hubSet {
	return &hubSet{apps: map[string]*appHub{}}
}

// get returns the app's hub or nil (counters and snapshots must not
// create hubs as a side effect).
func (hs *hubSet) get(appID string) *appHub {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.apps[appID]
}

// getOrCreate returns the app's hub, constructing an empty one on first use.
func (hs *hubSet) getOrCreate(appID string) *appHub {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	h := hs.apps[appID]
	if h == nil {
		h = &appHub{
			id:       appID,
			conns:    map[string]*hubConn{},
			channels: map[string]map[string]*hubConn{},
		}
		hs.apps[appID] = h
	}
	return h
}

// snapshotAll returns every live hub (for the /1.0/hub totals).
func (hs *hubSet) snapshotAll() []*appHub {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	out := make([]*appHub, 0, len(hs.apps))
	for _, h := range hs.apps {
		out = append(out, h)
	}
	return out
}

// teardownApp is the registry-lifecycle hook: DELETE and TTL reap tear the
// app's hub down (every socket closed through the internal mechanism,
// membership dropped, tombstone set for in-flight open bridges). Upstreams
// PUTs and Caddy reloads never reach here, deliberately.
func (hs *hubSet) teardownApp(appID string) {
	hs.mu.Lock()
	h := hs.apps[appID]
	delete(hs.apps, appID)
	hs.mu.Unlock()
	if h == nil {
		return
	}
	h.mu.Lock()
	h.tombstoned = true
	conns := make([]*hubConn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.closeWith(hubCloseGoingAway, "app deregistered")
	}
}

// hostsRemoved closes every connection bound through a host the app no
// longer claims (PATCH hosts); all other membership stays.
func (hs *hubSet) hostsRemoved(appID string, removed map[string]bool) {
	if len(removed) == 0 {
		return
	}
	h := hs.get(appID)
	if h == nil {
		return
	}
	h.mu.Lock()
	var victims []*hubConn
	for _, c := range h.conns {
		if removed[c.host] {
			victims = append(victims, c)
		}
	}
	h.mu.Unlock()
	for _, c := range victims {
		c.closeWith(hubCloseGoingAway, "host removed")
	}
}

// appHub is one app's hub: connections, channels, reservation count, and
// counters, under one lock.
type appHub struct {
	id string

	mu         sync.Mutex
	conns      map[string]*hubConn            // connection id → connection
	channels   map[string]map[string]*hubConn // channel → member set (mirrors conns[*].channels)
	reserved   int                            // slots held by in-flight open bridges
	tombstoned bool                           // set by teardown; in-flight opens recheck

	ctr hubCounters
}

// reserveSlot atomically checks reserved-plus-registered against the app's
// max_conns floor and holds one slot for an in-flight open bridge.
func (h *appHub) reserveSlot(floor int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tombstoned || len(h.conns)+h.reserved >= floor {
		return false
	}
	h.reserved++
	return true
}

func (h *appHub) releaseSlot() {
	h.mu.Lock()
	if h.reserved > 0 {
		h.reserved--
	}
	h.mu.Unlock()
}

// registerConn converts a reserved slot into a registered connection after
// the open bridge answered 2xx, rechecking the tombstone under the lock.
// false = the hub was torn down during the bridge; the caller releases
// nothing further (the slot is released here) and rejects the handshake.
func (h *appHub) registerConn(c *hubConn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reserved > 0 {
		h.reserved--
	}
	if h.tombstoned {
		return false
	}
	h.conns[c.id] = c
	return true
}

// removeConn drops the connection from conns and from every channel it is
// in (bidirectional maps, empty channels deleted). Idempotent.
func (h *appHub) removeConn(c *hubConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[c.id] != c {
		return
	}
	delete(h.conns, c.id)
	for ch := range c.channels {
		if set := h.channels[ch]; set != nil {
			delete(set, c.id)
			if len(set) == 0 {
				delete(h.channels, ch)
			}
		}
	}
	c.channels = map[string]struct{}{}
}

func (h *appHub) joinLocked(c *hubConn, ch string) {
	if _, ok := c.channels[ch]; ok {
		return // idempotent
	}
	set := h.channels[ch]
	if set == nil {
		set = map[string]*hubConn{}
		h.channels[ch] = set
	}
	set[c.id] = c
	c.channels[ch] = struct{}{}
}

func (h *appHub) leaveLocked(c *hubConn, ch string) {
	if _, ok := c.channels[ch]; !ok {
		return // idempotent
	}
	delete(c.channels, ch)
	if set := h.channels[ch]; set != nil {
		delete(set, c.id)
		if len(set) == 0 {
			delete(h.channels, ch)
		}
	}
}

// --- executor ---------------------------------------------------------------

// hubExecOutcome reports enqueue-time truth for one executed frame.
type hubExecOutcome struct {
	deliveries     int
	unknownTargets int
}

// execute runs the canonical execution order on one validated frame:
//
//  1. (already done by parseHubFrame) whole-frame grammar validation; here
//     the two stateful validations complete the stage: the client
//     sender-only membership rule and the max_channels simulation.
//  2. Apply +/- membership mutations in object-list order.
//  3. Resolve each object's @ delivery targets against post-mutation
//     membership.
//  4. Deliver event bundles (and kicks), then answer ?.
//
// origin is the originating connection: the sender (client plane), the
// bridged connection (bridge plane; may already be closed for close-bridge
// responses), or nil (publish). The whole frame executes under the hub
// lock — the linearization point for every mutation-versus-fan-out race.
func (h *appHub) execute(objs []hubObject, plane hubPlane, origin *hubConn) (hubExecOutcome, *hubViolation) {
	var out hubExecOutcome
	h.mu.Lock()

	// Stage 1 (stateful): validate before any effect.
	subjects, verr := h.validateMembershipLocked(objs, plane, origin)
	if verr != nil {
		h.mu.Unlock()
		return out, verr
	}

	// Stage 2: apply membership mutations in object-list order.
	for i, obj := range objs {
		for _, c := range subjects[i] {
			for _, ch := range obj.join {
				h.joinLocked(c, ch)
			}
			for _, ch := range obj.leave {
				h.leaveLocked(c, ch)
			}
		}
	}

	// Stages 3 and 4 per object: resolve against post-mutation membership,
	// then deliver.
	var pongs []json.RawMessage
	for _, obj := range objs {
		recipients := h.resolveDeliveryLocked(&obj, plane, origin, &out)
		if len(obj.events) > 0 {
			h.deliverLocked(&obj, plane, origin, recipients, &out)
		}
		if obj.kick != nil {
			kickFrame := []byte(`{"*":` + string(mustJSONString(*obj.kick)) + `}`)
			for _, rc := range recipients {
				if rc.enqueue(kickFrame) {
					rc.enqueueClose(hubCloseNormal, *obj.kick)
				}
			}
		}
		if obj.ping != nil {
			pongs = append(pongs, obj.ping)
		}
	}
	h.mu.Unlock()

	// The pong is sent after the frame executes.
	if origin != nil {
		for _, p := range pongs {
			var b bytes.Buffer
			b.WriteString(`{"!":`)
			b.Write(p)
			b.WriteByte('}')
			origin.enqueue(b.Bytes())
		}
	}

	h.ctr.unknownTargets.Add(int64(out.unknownTargets))
	h.ctr.deliveries.Add(int64(out.deliveries))
	return out, nil
}

// validateMembershipLocked completes stage-1 validation that needs state:
// the client sender-only rule and the max_channels simulation. It returns
// each object's resolved mutation-subject set (empty for objects without
// +/-). Trusted-plane channel subjects expand against post-prior-mutation
// membership, so the simulation applies mutations as it walks.
func (h *appHub) validateMembershipLocked(objs []hubObject, plane hubPlane, origin *hubConn) ([][]*hubConn, *hubViolation) {
	subjects := make([][]*hubConn, len(objs))

	// Lazily-copied simulation state: per-conn channel sets and per-channel
	// member sets, touched copies only.
	simChans := map[string]map[string]bool{}   // conn id → channel set
	simMembers := map[string]map[string]bool{} // channel → member ids
	connChans := func(c *hubConn) map[string]bool {
		if s, ok := simChans[c.id]; ok {
			return s
		}
		s := make(map[string]bool, len(c.channels))
		for ch := range c.channels {
			s[ch] = true
		}
		simChans[c.id] = s
		return s
	}
	members := func(ch string) map[string]bool {
		if s, ok := simMembers[ch]; ok {
			return s
		}
		set := h.channels[ch]
		s := make(map[string]bool, len(set))
		for id := range set {
			s[id] = true
		}
		simMembers[ch] = s
		return s
	}

	for i := range objs {
		obj := &objs[i]
		if len(obj.join) == 0 && len(obj.leave) == 0 {
			continue
		}

		var subs []*hubConn
		switch {
		case plane == hubPlaneClient:
			// Client +/- always mutate the sending connection. An @ that
			// resolves to anything other than exactly the sender rejects.
			if obj.hasAt {
				resolved := map[string]bool{}
				for _, t := range obj.at {
					if isHubChannel(t) {
						for id := range members(t) {
							resolved[id] = true
						}
					} else if _, live := h.conns[t]; live {
						resolved[t] = true
					}
				}
				if len(resolved) != 1 || !resolved[origin.id] {
					return nil, hubBad(i, `client "+" and "-" may mutate only the sending connection`)
				}
			}
			subs = []*hubConn{origin}

		case !obj.hasAt:
			// Bridge plane, @ absent → the originating connection (which
			// may be gone for a close-bridge response: skip, no subject).
			if origin != nil {
				if c, live := h.conns[origin.id]; live && c == origin {
					subs = []*hubConn{origin}
				}
			}

		default:
			// Trusted planes select membership subjects with @: a direct
			// id selects that live connection; a channel expands once to
			// its post-prior-mutation member set.
			seen := map[string]bool{}
			for _, t := range obj.at {
				if isHubChannel(t) {
					for id := range members(t) {
						if c, live := h.conns[id]; live && !seen[id] {
							seen[id] = true
							subs = append(subs, c)
						}
					}
				} else if c, live := h.conns[t]; live && !seen[t] {
					seen[t] = true
					subs = append(subs, c)
				}
			}
		}

		// Simulate this object's mutations, checking every resulting
		// per-connection channel count against the subject's own
		// max_channels before anything executes.
		for _, c := range subs {
			cs := connChans(c)
			for _, ch := range obj.join {
				if !cs[ch] {
					cs[ch] = true
					members(ch)[c.id] = true
					if len(cs) > c.maxChannels {
						return nil, hubBad(i, "join would exceed %d channels", c.maxChannels)
					}
				}
			}
			for _, ch := range obj.leave {
				if cs[ch] {
					delete(cs, ch)
					delete(members(ch), c.id)
				}
			}
		}
		subjects[i] = subs
	}
	return subjects, nil
}

// resolveDeliveryLocked resolves one object's @ (or the plane default)
// against post-mutation membership into a deduplicated recipient list.
// Missing channels and dead ids contribute no recipient and count.
func (h *appHub) resolveDeliveryLocked(obj *hubObject, plane hubPlane, origin *hubConn, out *hubExecOutcome) []*hubConn {
	// Nothing to deliver and nothing to kick → no resolution needed.
	if len(obj.events) == 0 && obj.kick == nil {
		return nil
	}
	var recipients []*hubConn
	seen := map[string]bool{}
	add := func(c *hubConn) {
		if !seen[c.id] {
			seen[c.id] = true
			recipients = append(recipients, c)
		}
	}
	if !obj.hasAt {
		// Plane default: the originating connection (publish always has @).
		if origin != nil {
			if c, live := h.conns[origin.id]; live && c == origin {
				add(c)
			} else {
				out.unknownTargets++
			}
		}
		return recipients
	}
	for _, t := range obj.at {
		if isHubChannel(t) {
			set := h.channels[t]
			if len(set) == 0 {
				out.unknownTargets++
				continue
			}
			for _, c := range set {
				add(c)
			}
		} else {
			c, live := h.conns[t]
			if !live {
				out.unknownTargets++
				continue
			}
			add(c)
		}
	}
	return recipients
}

// deliverLocked serializes the object's bundle variants once and enqueues
// shared bytes to each recipient. A bare event name excludes the
// originating connection; the ! suffix includes it (and is stripped on
// delivery). Publish has no originating connection, so bare and !
// spellings deliver identically there; exclusion never keys off <.
func (h *appHub) deliverLocked(obj *hubObject, plane hubPlane, origin *hubConn, recipients []*hubConn, out *hubExecOutcome) {
	var prov []string
	hasProv := false
	if plane == hubPlaneClient {
		prov = []string{origin.id}
		hasProv = true
	} else if obj.hasProv {
		prov = obj.prov
		hasProv = true
	}

	full := serializeHubBundle(prov, hasProv, obj.events, false)
	var senderOnly []byte // events the sender still receives (!-suffixed)
	if origin != nil {
		includes := 0
		for _, ev := range obj.events {
			if ev.include {
				includes++
			}
		}
		if includes == len(obj.events) {
			senderOnly = full
		} else if includes > 0 {
			senderOnly = serializeHubBundle(prov, hasProv, obj.events, true)
		}
	}

	for _, rc := range recipients {
		bundle := full
		if origin != nil && rc.id == origin.id {
			bundle = senderOnly
			if bundle == nil {
				continue // every event excluded the sender: zero delivery, legal
			}
		}
		if rc.enqueue(bundle) {
			out.deliveries++
		} else {
			// Racing close: the queue is gone — dropped and counted,
			// never an error.
			out.unknownTargets++
		}
	}
}

// serializeHubBundle builds one delivered frame: < when present, then the
// event keys (suffix stripped) with their value bytes exactly as received.
// No @, no +/-, no ?, no transport metadata.
func serializeHubBundle(prov []string, hasProv bool, events []hubEvent, includeOnly bool) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	if hasProv {
		b.WriteString(`"<":`)
		pj, _ := json.Marshal(prov)
		b.Write(pj)
		first = false
	}
	for _, ev := range events {
		if includeOnly && !ev.include {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(strconv.Quote(ev.name))
		b.WriteByte(':')
		b.Write(ev.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func isHubChannel(target string) bool {
	return len(target) > 0 && target[0] == '/'
}

func mustJSONString(s string) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("marshaling string: %v", err))
	}
	return b
}

// --- snapshots ----------------------------------------------------------------

// hubStatsBucket is the JSON counter block for /1.0/hub, process totals
// and per app.
type hubStatsBucket struct {
	Conns          int   `json:"conns"`
	Channels       int   `json:"channels"`
	FramesIn       int64 `json:"frames_in"`
	Deliveries     int64 `json:"deliveries"`
	Publishes      int64 `json:"publishes"`
	RejectedFrames int64 `json:"rejected_frames"`
	UnknownTargets int64 `json:"unknown_targets"`
	SlowCloses     int64 `json:"slow_closes"`
	PingCloses     int64 `json:"ping_closes"`
	BridgeSent     int64 `json:"bridge_sent"`
	BridgeFailed   int64 `json:"bridge_failed"`
	BridgeDropped  int64 `json:"bridge_dropped"`
	BridgeGarbage  int64 `json:"bridge_garbage"`
}

func (b *hubStatsBucket) add(h *appHub) {
	h.mu.Lock()
	b.Conns += len(h.conns)
	b.Channels += len(h.channels)
	h.mu.Unlock()
	b.FramesIn += h.ctr.framesIn.Load()
	b.Deliveries += h.ctr.deliveries.Load()
	b.Publishes += h.ctr.publishes.Load()
	b.RejectedFrames += h.ctr.rejectedFrames.Load()
	b.UnknownTargets += h.ctr.unknownTargets.Load()
	b.SlowCloses += h.ctr.slowCloses.Load()
	b.PingCloses += h.ctr.pingCloses.Load()
	b.BridgeSent += h.ctr.bridgeSent.Load()
	b.BridgeFailed += h.ctr.bridgeFailed.Load()
	b.BridgeDropped += h.ctr.bridgeDropped.Load()
	b.BridgeGarbage += h.ctr.bridgeGarbage.Load()
}

// hubStats is the GET /1.0/hub response body.
type hubStats struct {
	hubStatsBucket
	Apps map[string]*hubStatsBucket `json:"apps"`
}

func (hs *hubSet) stats() hubStats {
	out := hubStats{Apps: map[string]*hubStatsBucket{}}
	for _, h := range hs.snapshotAll() {
		out.add(h)
		b := &hubStatsBucket{}
		b.add(h)
		out.Apps[h.id] = b
	}
	return out
}

// hubMembershipSnapshot is the GET /1.0/apps/{id}/hub response body: the
// tenant's resync instrument. Connections are keyed by opaque snapshot
// handles — never raw connection ids, never usable as wire targets.
type hubMembershipSnapshot struct {
	Conns       int                 `json:"conns"`
	Channels    map[string]int      `json:"channels"`
	Connections map[string][]string `json:"connections"`
}

func (h *appHub) membershipSnapshot() hubMembershipSnapshot {
	out := hubMembershipSnapshot{
		Channels:    map[string]int{},
		Connections: map[string][]string{},
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out.Conns = len(h.conns)
	for ch, set := range h.channels {
		out.Channels[ch] = len(set)
	}
	i := 0
	for _, c := range h.conns {
		i++
		chans := make([]string, 0, len(c.channels))
		for ch := range c.channels {
			chans = append(chans, ch)
		}
		out.Connections["conn-"+strconv.Itoa(i)] = chans
	}
	return out
}
