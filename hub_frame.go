package janus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Hub wire grammar (docs/20260720-162350-hub-design.md "Wire grammar").
//
// A frame is one JSON object or a JSON array of objects. Every key is
// exactly one of: a sigil (@ + - < > ? ! *) or an event name. One grammar
// serves three planes (client, bridge response, publish) with per-plane
// policy from the sigil table; anything a plane may not say is rejected
// loudly, never stripped. Whole-frame validation precedes every effect.

// hubPlane names the directive source; policy is per plane.
type hubPlane int

const (
	hubPlaneClient  hubPlane = iota // WebSocket text frame (untrusted)
	hubPlaneBridge                  // tenant's 2xx bridge-response body (trusted)
	hubPlanePublish                 // POST /1.0/apps/{id}/hub/publish (trusted)
)

func (p hubPlane) String() string {
	switch p {
	case hubPlaneClient:
		return "client"
	case hubPlaneBridge:
		return "bridge"
	default:
		return "publish"
	}
}

// WebSocket close codes the hub speaks (RFC 6455).
const (
	hubCloseNormal      = 1000 // trusted * kick
	hubCloseGoingAway   = 1001 // teardown, ping timeout, host removed, hub disabled
	hubCloseUnsupported = 1003 // binary frame
	hubClosePolicy      = 1008 // malformed / plane-violating frame
	hubCloseTooBig      = 1009 // frame over max_frame
	hubCloseTryLater    = 1013 // slow consumer
)

// Structural limits (fixed in v1; "Size and shape limits").
const (
	hubMaxObjects   = 16  // objects per list frame
	hubMaxTargets   = 64  // targets per @ list
	hubMaxOpChans   = 64  // channels per + / - list
	hubMaxEventKeys = 16  // event keys per object
	hubMaxChanName  = 128 // channel name bytes
	hubMaxPingBytes = 128 // ? value string bytes
	hubMaxKickBytes = 128 // * value string bytes
)

var (
	// hubChannelRE: one or more non-empty /-prefixed segments of URL-safe
	// characters; no empty segments, no trailing slash.
	hubChannelRE = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)

	// hubEventRE: starts with a letter or underscore (so no event name can
	// collide with a current or future sigil), ≤64 bytes before the
	// optional include-sender "!" suffix. "chat!!" fails here.
	hubEventRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]{0,63}!?$`)
)

// hubViolation is one loud rejection: the close code names the class, the
// message is the precise positioned error (position first, so the RFC 6455
// 123-byte reason cap never truncates the identifying part). The client
// plane closes with it; publish answers 400 with it; a bridge response is
// dropped whole as tenant garbage with it.
type hubViolation struct {
	code int
	msg  string
}

func (v *hubViolation) Error() string { return v.msg }

func hubBad(item int, format string, args ...any) *hubViolation {
	return &hubViolation{code: hubClosePolicy, msg: fmt.Sprintf("item %d: ", item) + fmt.Sprintf(format, args...)}
}

func hubBadFrame(format string, args ...any) *hubViolation {
	return &hubViolation{code: hubClosePolicy, msg: fmt.Sprintf(format, args...)}
}

// hubEvent is one event key of an object: the stripped name, whether the
// wire spelling carried the include-sender "!" suffix, and the value bytes
// exactly as received (never interpreted, never re-serialized).
type hubEvent struct {
	name    string
	include bool
	value   json.RawMessage
}

// hubObject is one validated directive object.
type hubObject struct {
	at    []string // @ delivery targets; nil when absent
	hasAt bool

	join  []string // + channels
	leave []string // - channels

	prov    []string // < provenance list (trusted planes)
	hasProv bool

	ping json.RawMessage // raw ? value (a JSON string), echoed verbatim; nil = absent
	kick *string         // * reason (trusted planes)

	events []hubEvent
}

// hasEffect reports whether the object does any work; a routing- or
// provenance-only object is a no-op frame and rejects loudly.
func (o *hubObject) hasEffect() bool {
	return len(o.events) > 0 || len(o.join) > 0 || len(o.leave) > 0 || o.kick != nil || o.ping != nil
}

// hubKV is one top-level key of a directive object in encounter order with
// its raw value bytes.
type hubKV struct {
	key string
	val json.RawMessage
}

// parseHubFrame validates one whole frame for the plane and returns its
// objects in list order. Any violation rejects the whole frame before any
// effect; nothing is repaired, clipped, or partially applied.
func parseHubFrame(data []byte, plane hubPlane) ([]hubObject, *hubViolation) {
	rawObjs, verr := splitHubFrame(data)
	if verr != nil {
		return nil, verr
	}

	objs := make([]hubObject, 0, len(rawObjs))
	// Bare and !-suffixed spellings of one event name must not coexist
	// anywhere in the frame (delivery strips the suffix; stripping both
	// to one key would make the frame's meaning representation-dependent).
	bareAt := map[string]int{}
	suffAt := map[string]int{}
	for i, kvs := range rawObjs {
		obj, verr := parseHubObject(kvs, i, plane)
		if verr != nil {
			return nil, verr
		}
		for _, ev := range obj.events {
			if ev.include {
				if j, dup := bareAt[ev.name]; dup {
					return nil, hubEventSpellings(j, i, ev.name)
				}
				if _, ok := suffAt[ev.name]; !ok {
					suffAt[ev.name] = i
				}
			} else {
				if j, dup := suffAt[ev.name]; dup {
					return nil, hubEventSpellings(i, j, ev.name)
				}
				if _, ok := bareAt[ev.name]; !ok {
					bareAt[ev.name] = i
				}
			}
		}
		objs = append(objs, obj)
	}
	return objs, nil
}

func hubEventSpellings(bareItem, suffItem int, name string) *hubViolation {
	if bareItem == suffItem {
		return hubBad(bareItem, "event %q appears as both %q and %q", name, name, name+"!")
	}
	lo, hi := bareItem, suffItem
	if hi < lo {
		lo, hi = hi, lo
	}
	return hubBadFrame("items %d and %d: event %q appears as both %q and %q", lo, hi, name, name, name+"!")
}

// splitHubFrame walks the frame's tokens once: it enforces
// object-or-list-of-objects shape, rejects duplicate object keys at every
// nesting level, bounds the object count, and captures each top-level
// key's raw value bytes in encounter order.
func splitHubFrame(data []byte) ([][]hubKV, *hubViolation) {
	notAFrame := hubBadFrame("frame is not a JSON object or list of objects")
	if !utf8.Valid(data) {
		return nil, notAFrame
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, notAFrame
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil, notAFrame
	}

	var out [][]hubKV
	switch d {
	case '{':
		kvs, verr := consumeHubObject(dec, data, 0)
		if verr != nil {
			return nil, verr
		}
		out = [][]hubKV{kvs}
	case '[':
		for dec.More() {
			if len(out) >= hubMaxObjects {
				return nil, hubBadFrame("frame has more than %d objects (max %d)", hubMaxObjects, hubMaxObjects)
			}
			et, err := dec.Token()
			if err != nil {
				return nil, notAFrame
			}
			if ed, ok := et.(json.Delim); !ok || ed != '{' {
				return nil, notAFrame
			}
			kvs, verr := consumeHubObject(dec, data, len(out))
			if verr != nil {
				return nil, verr
			}
			out = append(out, kvs)
		}
		if _, err := dec.Token(); err != nil { // consume ']'
			return nil, notAFrame
		}
		if len(out) == 0 {
			return nil, hubBadFrame("frame is an empty list")
		}
	default:
		return nil, notAFrame
	}

	if _, err := dec.Token(); err != io.EOF {
		return nil, hubBadFrame("frame has trailing data after the JSON value")
	}
	return out, nil
}

// consumeHubObject reads one object's key/value pairs (the '{' token is
// already consumed), rejecting duplicate keys and capturing raw values.
func consumeHubObject(dec *json.Decoder, data []byte, item int) ([]hubKV, *hubViolation) {
	var kvs []hubKV
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, hubBadFrame("frame is not a JSON object or list of objects")
		}
		key, ok := kt.(string)
		if !ok {
			return nil, hubBadFrame("frame is not a JSON object or list of objects")
		}
		if seen[key] {
			return nil, hubBad(item, "duplicate key %q", key)
		}
		seen[key] = true
		valStart := hubSkipToValue(data, dec.InputOffset())
		if verr := consumeHubValue(dec, item); verr != nil {
			return nil, verr
		}
		valEnd := dec.InputOffset()
		if valStart >= valEnd {
			return nil, hubBadFrame("frame is not a JSON object or list of objects")
		}
		kvs = append(kvs, hubKV{key: key, val: json.RawMessage(data[valStart:valEnd])})
	}
	if _, err := dec.Token(); err != nil { // consume '}'
		return nil, hubBadFrame("frame is not a JSON object or list of objects")
	}
	return kvs, nil
}

// hubSkipToValue advances past the whitespace and colon that separate an
// object key from its value; a JSON value can begin with neither.
func hubSkipToValue(data []byte, off int64) int64 {
	i := off
	for i < int64(len(data)) {
		switch data[i] {
		case ' ', '\t', '\r', '\n', ':':
			i++
		default:
			return i
		}
	}
	return i
}

// consumeHubValue consumes one JSON value's tokens, rejecting duplicate
// object keys at any nesting depth ("the parser also rejects duplicate
// object keys at any nesting level").
func consumeHubValue(dec *json.Decoder, item int) *hubViolation {
	tok, err := dec.Token()
	if err != nil {
		return hubBadFrame("frame is not a JSON object or list of objects")
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil // scalar
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return hubBadFrame("frame is not a JSON object or list of objects")
			}
			key, ok := kt.(string)
			if !ok {
				return hubBadFrame("frame is not a JSON object or list of objects")
			}
			if seen[key] {
				return hubBad(item, "duplicate key %q", key)
			}
			seen[key] = true
			if verr := consumeHubValue(dec, item); verr != nil {
				return verr
			}
		}
		if _, err := dec.Token(); err != nil {
			return hubBadFrame("frame is not a JSON object or list of objects")
		}
	case '[':
		for dec.More() {
			if verr := consumeHubValue(dec, item); verr != nil {
				return verr
			}
		}
		if _, err := dec.Token(); err != nil {
			return hubBadFrame("frame is not a JSON object or list of objects")
		}
	}
	return nil
}

// parseHubObject applies the sigil table (per-plane policy), value shapes,
// name grammars, and structural limits to one object.
func parseHubObject(kvs []hubKV, item int, plane hubPlane) (hubObject, *hubViolation) {
	var obj hubObject
	for _, kv := range kvs {
		switch kv.key {
		case "@":
			arr, verr := hubStringArray(item, "@", kv.val)
			if verr != nil {
				return obj, verr
			}
			if len(arr) == 0 {
				return obj, hubBad(item, `"@" must contain at least one target`)
			}
			if len(arr) > hubMaxTargets {
				return obj, hubBad(item, `"@" has %d targets (max %d)`, len(arr), hubMaxTargets)
			}
			for _, t := range arr {
				if strings.HasPrefix(t, "/") {
					if verr := hubCheckChannel(item, "@", t); verr != nil {
						return obj, verr
					}
				}
			}
			obj.at = arr
			obj.hasAt = true

		case "+", "-":
			arr, verr := hubStringArray(item, kv.key, kv.val)
			if verr != nil {
				return obj, verr
			}
			if len(arr) > hubMaxOpChans {
				return obj, hubBad(item, "%q has %d channels (max %d)", kv.key, len(arr), hubMaxOpChans)
			}
			for _, ch := range arr {
				if !strings.HasPrefix(ch, "/") {
					return obj, hubBad(item, "%q entry %q is not a channel (want /-prefix)", kv.key, ch)
				}
				if verr := hubCheckChannel(item, kv.key, ch); verr != nil {
					return obj, verr
				}
			}
			if kv.key == "+" {
				obj.join = arr
			} else {
				obj.leave = arr
			}

		case "<":
			if plane == hubPlaneClient {
				return obj, hubBad(item, `"<" is stamped by janus; clients never send it`)
			}
			arr, verr := hubStringArray(item, "<", kv.val)
			if verr != nil {
				return obj, verr
			}
			obj.prov = arr
			obj.hasProv = true

		case ">":
			return obj, hubBad(item, `">" is reserved`)

		case "?":
			if plane != hubPlaneClient {
				return obj, hubBad(item, `"?" is client-to-janus only`)
			}
			if verr := hubCheckShortString(item, "?", kv.val, hubMaxPingBytes); verr != nil {
				return obj, verr
			}
			obj.ping = kv.val

		case "!":
			return obj, hubBad(item, `"!" is janus-to-client only`)

		case "*":
			if plane == hubPlaneClient {
				return obj, hubBad(item, `"*" is delivery-direction only`)
			}
			if verr := hubCheckShortString(item, "*", kv.val, hubMaxKickBytes); verr != nil {
				return obj, verr
			}
			var reason string
			_ = json.Unmarshal(kv.val, &reason) // shape verified above
			obj.kick = &reason

		default:
			if !hubEventRE.MatchString(kv.key) {
				return obj, hubBad(item, "key %q is not a sigil or legal event name", kv.key)
			}
			if len(obj.events) >= hubMaxEventKeys {
				return obj, hubBad(item, "object has more than %d event keys (max %d)", hubMaxEventKeys, hubMaxEventKeys)
			}
			name, include := strings.CutSuffix(kv.key, "!")
			obj.events = append(obj.events, hubEvent{name: name, include: include, value: kv.val})
		}
	}

	if plane == hubPlanePublish && !obj.hasAt {
		return obj, hubBad(item, `"@" is required on the publish plane`)
	}
	if !obj.hasEffect() {
		return obj, hubBad(item, "frame has no event, membership mutation, kick, or ping")
	}
	return obj, nil
}

// hubStringArray parses a sigil value that must be a JSON array of strings.
func hubStringArray(item int, key string, raw json.RawMessage) ([]string, *hubViolation) {
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil || arr == nil {
		return nil, hubBad(item, "%q must be an array of strings", key)
	}
	return arr, nil
}

// hubCheckShortString enforces "JSON string ≤ maxBytes" for ? and * values.
func hubCheckShortString(item int, key string, raw json.RawMessage, maxBytes int) *hubViolation {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return hubBad(item, "%q must be a JSON string of at most %d bytes", key, maxBytes)
	}
	if len(s) > maxBytes {
		return hubBad(item, "%q must be a JSON string of at most %d bytes", key, maxBytes)
	}
	return nil
}

// hubCheckChannel enforces the channel-name grammar and length cap for one
// /-prefixed entry.
func hubCheckChannel(item int, key, ch string) *hubViolation {
	if len(ch) > hubMaxChanName {
		return hubBad(item, "%q entry %q exceeds %d bytes", key, ch, hubMaxChanName)
	}
	if !hubChannelRE.MatchString(ch) {
		return hubBad(item, "%q entry %q is not a legal channel name", key, ch)
	}
	return nil
}
