package janus

import (
	"strings"
	"testing"
)

// --- frame validation table ---------------------------------------------------
// One test row per hard-error contract row, plus accepts for every legal
// example in the design doc.

func TestHubFrameHardErrors(t *testing.T) {
	tests := []struct {
		name  string
		plane hubPlane
		frame string
		code  int
		msg   string // substring the positioned reason must carry
	}{
		{"not json", hubPlaneClient, `nope`, hubClosePolicy, "not a JSON object or list"},
		{"string frame", hubPlaneClient, `"hello"`, hubClosePolicy, "not a JSON object or list"},
		{"number frame", hubPlaneClient, `42`, hubClosePolicy, "not a JSON object or list"},
		{"bare array of non-objects", hubPlaneClient, `[1,2]`, hubClosePolicy, "not a JSON object or list"},
		{"empty list", hubPlaneClient, `[]`, hubClosePolicy, "empty list"},
		{"trailing data", hubPlaneClient, `{"chat":{}} {"x":1}`, hubClosePolicy, "trailing data"},
		{"invalid utf8", hubPlaneClient, "{\"chat\":\"\xff\xfe\"}", hubClosePolicy, "not a JSON object or list"},

		{"client-supplied <", hubPlaneClient, `{"<":["x"],"chat":{}}`, hubClosePolicy, `item 0: "<" is stamped by janus`},
		{"reserved > client", hubPlaneClient, `{">":["x"],"chat":{}}`, hubClosePolicy, `item 0: ">" is reserved`},
		{"reserved > bridge", hubPlaneBridge, `{">":["x"],"chat":{}}`, hubClosePolicy, `">" is reserved`},
		{"reserved > publish", hubPlanePublish, `{"@":["/r"],">":["x"],"chat":{}}`, hubClosePolicy, `">" is reserved`},
		{"exact ! client", hubPlaneClient, `{"!":"t1"}`, hubClosePolicy, `"!" is janus-to-client only`},
		{"exact ! bridge", hubPlaneBridge, `{"!":"t1"}`, hubClosePolicy, `"!" is janus-to-client only`},
		{"exact ! publish", hubPlanePublish, `{"@":["/r"],"!":"t1"}`, hubClosePolicy, `"!" is janus-to-client only`},
		{"client *", hubPlaneClient, `{"*":"bye"}`, hubClosePolicy, `"*" is delivery-direction only`},
		{"bridge ?", hubPlaneBridge, `{"?":"t1"}`, hubClosePolicy, `"?" is client-to-janus only`},
		{"publish ?", hubPlanePublish, `{"@":["/r"],"?":"t1"}`, hubClosePolicy, `"?" is client-to-janus only`},

		{"@ not array", hubPlaneClient, `{"@":"lobby","chat":{}}`, hubClosePolicy, `"@" must be an array of strings`},
		{"@ mixed types", hubPlaneClient, `{"@":["/x",3],"chat":{}}`, hubClosePolicy, `"@" must be an array of strings`},
		{"@ null", hubPlaneClient, `{"@":null,"chat":{}}`, hubClosePolicy, `"@" must be an array of strings`},
		{"+ not array", hubPlaneClient, `{"+":"/lobby"}`, hubClosePolicy, `"+" must be an array of strings`},
		{"? not string", hubPlaneClient, `{"?":42}`, hubClosePolicy, `"?" must be a JSON string`},
		{"? too long", hubPlaneClient, `{"?":"` + strings.Repeat("x", 129) + `"}`, hubClosePolicy, `"?" must be a JSON string of at most 128 bytes`},
		{"* not string", hubPlaneBridge, `{"*":42}`, hubClosePolicy, `"*" must be a JSON string`},
		{"* too long", hubPlanePublish, `{"@":["/r"],"*":"` + strings.Repeat("x", 129) + `"}`, hubClosePolicy, `"*" must be a JSON string of at most 128 bytes`},
		{"< not array", hubPlaneBridge, `{"<":"me","chat":{}}`, hubClosePolicy, `"<" must be an array of strings`},

		{"present-empty @ client", hubPlaneClient, `{"@":[],"chat":{}}`, hubClosePolicy, `item 0: "@" must contain at least one target`},
		{"present-empty @ bridge", hubPlaneBridge, `{"@":[],"chat":{}}`, hubClosePolicy, `"@" must contain at least one target`},
		{"present-empty @ publish", hubPlanePublish, `{"@":[],"chat":{}}`, hubClosePolicy, `"@" must contain at least one target`},
		{"absent @ publish", hubPlanePublish, `{"chat":{}}`, hubClosePolicy, `"@" is required on the publish plane`},

		{"join bare name", hubPlaneClient, `{"+":["lobby"]}`, hubClosePolicy, `"+" entry "lobby" is not a channel (want /-prefix)`},
		{"leave bare name", hubPlaneClient, `{"-":["room"]}`, hubClosePolicy, `"-" entry "room" is not a channel`},
		{"join empty segment", hubPlaneClient, `{"+":["/a//b"]}`, hubClosePolicy, `not a legal channel name`},
		{"join trailing slash", hubPlaneClient, `{"+":["/a/"]}`, hubClosePolicy, `not a legal channel name`},
		{"join bad char", hubPlaneClient, `{"+":["/a b"]}`, hubClosePolicy, `not a legal channel name`},
		{"join wildcard", hubPlaneClient, `{"+":["/game/*"]}`, hubClosePolicy, `not a legal channel name`},
		{"channel too long", hubPlaneClient, `{"+":["/` + strings.Repeat("x", 128) + `"]}`, hubClosePolicy, "exceeds 128 bytes"},
		{"@ channel illegal", hubPlaneClient, `{"@":["/a//b"],"chat":{}}`, hubClosePolicy, `not a legal channel name`},

		{"illegal event name", hubPlaneClient, `{"9lives":{}}`, hubClosePolicy, `key "9lives" is not a sigil or legal event name`},
		{"double bang", hubPlaneClient, `{"chat!!":{}}`, hubClosePolicy, `key "chat!!" is not a sigil or legal event name`},
		{"bang prefix", hubPlaneClient, `{"!chat":{}}`, hubClosePolicy, `key "!chat" is not a sigil or legal event name`},
		{"event name too long", hubPlaneClient, `{"` + "e" + strings.Repeat("x", 64) + `":{}}`, hubClosePolicy, "not a sigil or legal event name"},

		{"duplicate key", hubPlaneClient, `{"chat":1,"chat":2}`, hubClosePolicy, `item 0: duplicate key "chat"`},
		{"nested duplicate key", hubPlaneClient, `{"chat":{"a":1,"a":2}}`, hubClosePolicy, `duplicate key "a"`},
		{"deep nested duplicate", hubPlaneClient, `{"chat":{"a":[{"b":1,"b":2}]}}`, hubClosePolicy, `duplicate key "b"`},

		{"bare and suffixed same object", hubPlaneClient, `{"@":["/r"],"chat":{},"chat!":{}}`, hubClosePolicy, `event "chat" appears as both "chat" and "chat!"`},
		{"bare and suffixed across objects", hubPlaneClient, `[{"@":["/r"],"chat":{}},{"+":["/x"]},{"@":["/r"],"chat!":{}}]`, hubClosePolicy, `items 0 and 2: event "chat" appears as both "chat" and "chat!"`},

		{"routing-only no-op", hubPlaneClient, `{"@":["/r"]}`, hubClosePolicy, `item 0: frame has no event, membership mutation, kick, or ping`},
		{"provenance-only no-op", hubPlaneBridge, `{"<":["relay"]}`, hubClosePolicy, "frame has no event, membership mutation, kick, or ping"},

		{"too many objects", hubPlaneClient, `[` + strings.Repeat(`{"+":["/x"]},`, 16) + `{"+":["/x"]}]`, hubClosePolicy, "more than 16 objects"},
		{"too many targets", hubPlaneClient, `{"@":[` + repeatJSON(`"/c%d"`, 65) + `],"chat":{}}`, hubClosePolicy, `"@" has 65 targets (max 64)`},
		{"too many join channels", hubPlaneClient, `{"+":[` + repeatJSON(`"/c%d"`, 65) + `]}`, hubClosePolicy, `"+" has 65 channels (max 64)`},
		{"too many event keys", hubPlaneClient, `{` + eventKeys(17) + `}`, hubClosePolicy, "more than 16 event keys"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, verr := parseHubFrame([]byte(tt.frame), tt.plane)
			if verr == nil {
				t.Fatalf("want rejection %q, got accept", tt.msg)
			}
			if verr.code != tt.code {
				t.Fatalf("close code: want %d, got %d (%s)", tt.code, verr.code, verr.msg)
			}
			if !strings.Contains(verr.msg, tt.msg) {
				t.Fatalf("reason: want substring %q, got %q", tt.msg, verr.msg)
			}
		})
	}
}

func repeatJSON(pattern string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = strings.ReplaceAll(pattern, "%d", string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	return strings.Join(parts, ",")
}

func eventKeys(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = `"ev` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `":{}`
	}
	return strings.Join(parts, ",")
}

func TestHubFrameAccepts(t *testing.T) {
	tests := []struct {
		name  string
		plane hubPlane
		frame string
	}{
		// Every example client frame from the design doc.
		{"join two channels", hubPlaneClient, `{"+": ["/lobby", "/game/42"]}`},
		{"event to channel", hubPlaneClient, `{"@": ["/game/42"], "move": {"x": 3, "y": 5}}`},
		{"include-sender event", hubPlaneClient, `{"@": ["/game/42"], "move!": {"x": 3, "y": 5}}`},
		{"direct id whisper", hubPlaneClient, `{"@": ["k7f2m9x0q4w1z8p3"], "whisper": {"text": "hi"}}`},
		{"ping", hubPlaneClient, `{"?": "t1721512345"}`},
		{"list frame", hubPlaneClient, `[{"-": ["/lobby"]}, {"@": ["/game/42"], "left": {"who": "…"}}]`},

		{"nested channel name", hubPlaneClient, `{"+":["/friends/high-school/math-team"]}`},
		{"underscore event", hubPlaneClient, `{"_private":{}}`},
		{"event with dots and dashes", hubPlaneClient, `{"user.profile-update":1}`},
		{"64-byte event name", hubPlaneClient, `{"` + "e" + strings.Repeat("x", 63) + `":{}}`},
		{"ping alongside directives", hubPlaneClient, `{"+":["/x"],"?":"t"}`},
		{"arbitrary event values", hubPlaneClient, `{"chat":[1,"two",{"three":3},null,true]}`},

		{"bridge enroll", hubPlaneBridge, `{"+": ["/user/42", "/lobby"]}`},
		{"bridge announce", hubPlaneBridge, `{"@": ["/lobby"], "joined": {"who": "…"}}`},
		{"bridge provenance", hubPlaneBridge, `{"<":["svc-1"],"chat":{}}`},
		{"bridge kick", hubPlaneBridge, `{"*":"session expired"}`},
		{"bridge cross-conn join", hubPlaneBridge, `{"@":["k7f2m9x0q4w1z8p3"],"+":["/vip"]}`},

		{"publish event", hubPlanePublish, `{"@":["/room"],"chat":{}}`},
		{"publish provenance", hubPlanePublish, `{"<":["bampub"],"@":["/room"],"news":{}}`},
		{"publish kick", hubPlanePublish, `{"@":["k7f2m9x0q4w1z8p3"],"*":"kicked"}`},
		{"publish list", hubPlanePublish, `[{"@":["/a"],"x":1},{"@":["/b"],"y":2}]`},

		{"limit: 16 objects", hubPlaneClient, `[` + strings.TrimSuffix(strings.Repeat(`{"+":["/x"]},`, 16), ",") + `]`},
		{"limit: 64 targets", hubPlaneClient, `{"@":[` + repeatJSON(`"/c%d"`, 64) + `],"chat":{}}`},
		{"limit: 16 event keys", hubPlaneClient, `{` + eventKeys(16) + `}`},
		{"limit: 128-byte ? value", hubPlaneClient, `{"?":"` + strings.Repeat("x", 128) + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, verr := parseHubFrame([]byte(tt.frame), tt.plane); verr != nil {
				t.Fatalf("want accept, got %q", verr.msg)
			}
		})
	}
}

// --- grammar edges --------------------------------------------------------------

func TestHubGrammarEdges(t *testing.T) {
	// Sigil-vs-event-name boundary: "!" alone is the pong sigil (rejected
	// from clients); "chat!" is an event spelling; "!chat" is illegal.
	if _, verr := parseHubFrame([]byte(`{"!":"x"}`), hubPlaneClient); verr == nil {
		t.Fatal(`exact "!" must reject`)
	}
	objs, verr := parseHubFrame([]byte(`{"chat!":{}}`), hubPlaneClient)
	if verr != nil {
		t.Fatalf("chat! must parse: %v", verr)
	}
	if objs[0].events[0].name != "chat" || !objs[0].events[0].include {
		t.Fatalf("chat! must strip to chat+include, got %+v", objs[0].events[0])
	}
	if _, verr := parseHubFrame([]byte(`{"!chat":{}}`), hubPlaneClient); verr == nil {
		t.Fatal(`"!chat" must reject`)
	}

	// Channel grammar.
	for _, good := range []string{"/a", "/a/b", "/user/42", "/a-b_c", "/x.y~z"} {
		if !hubChannelRE.MatchString(good) {
			t.Errorf("channel %q must match", good)
		}
	}
	for _, bad := range []string{"a", "/", "//", "/a//b", "/a/", "/a b", "/a$b", ""} {
		if hubChannelRE.MatchString(bad) {
			t.Errorf("channel %q must not match", bad)
		}
	}

	// Event grammar: sigils can never be legal event names.
	for _, sigil := range []string{"@", "+", "-", "<", ">", "?", "!", "*"} {
		if hubEventRE.MatchString(sigil) {
			t.Errorf("sigil %q must not be a legal event name", sigil)
		}
	}

	// Event values are byte-preserved, never re-serialized.
	frame := `{"chat":{"z":1,  "a": [2,3]}}`
	objs, verr = parseHubFrame([]byte(frame), hubPlaneClient)
	if verr != nil {
		t.Fatal(verr)
	}
	if got := string(objs[0].events[0].value); got != `{"z":1,  "a": [2,3]}` {
		t.Fatalf("value bytes altered: %q", got)
	}
}

// TestHubViolationPositionFirst pins that reasons put the position and rule
// before anything client-controlled, so the RFC 6455 123-byte truncation
// never costs the identifying part.
func TestHubViolationPositionFirst(t *testing.T) {
	long := strings.Repeat("x", 300)
	_, verr := parseHubFrame([]byte(`{"`+long+`":1}`), hubPlaneClient)
	if verr == nil {
		t.Fatal("want rejection")
	}
	if !strings.HasPrefix(verr.msg, "item 0: key ") {
		t.Fatalf("position must lead: %q", verr.msg)
	}
}
