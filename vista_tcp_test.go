package janus

// Capability 11's extension to a stock RPC Broker over TCP
// (docs/20261114-142034-capability-vista-tcp.md): the target grammar, the
// dial, the message guard, the failed sign-on limit, and the broker's own
// endings, against the fake broker (newFakeBroker in vista_fakeem_test.go).

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The guard's replies, byte for byte: LEN, the message, \0, \x04.
var (
	tcpReplyInvalid      = []byte("\x0fInvalid request\x00\x04")
	tcpReplyUnauthorized = []byte("\x14Unauthorized request\x00\x04")
	tcpReplyHeld         = []byte("\x29Too many failed sign-ons; try again later\x00\x04")
)

func newVistaTCPRig(t *testing.T, logger *zap.Logger, edit func(*vistaLimits)) *vistaRig {
	t.Helper()
	b := newFakeBroker(t)
	return newVistaRigTarget(t, b, "tcp/"+b.Addr(), logger, edit)
}

// signon signs the session on with the fake's good code and returns the
// rotated token.
func (g *vistaRig) signon(tok string) string {
	g.t.Helper()
	r := g.do(xwbRPCMsg("XUS AV CODE", " GOOD;CODE "), bearer(tok)...)
	fresh := vistaSetToken(r.h)
	if r.code != http.StatusOK || !bytes.Equal(r.body, emReplyAVGood) || fresh == "" {
		g.t.Fatalf("sign-on: %d %q token %q", r.code, r.body, fresh)
	}
	return fresh
}

// received is how many messages the fake's connections have received.
func (g *vistaRig) received() int {
	n := 0
	for _, c := range g.em.Conns() {
		n += len(c.Received())
	}
	return n
}

// --- message builders ------------------------------------------------------------

// xp is one encoded parameter for a given length width.
type xp func(w int) string

func pLit(v string) xp { return func(w int) string { return "0" + fakeEmLong(v, w) + "f" } }
func pRef(v string) xp { return func(w int) string { return "1" + fakeEmLong(v, w) + "f" } }

// pList is a list (type 2) of subscript, value pairs; an empty subscript
// has no value.
func pList(kv ...string) xp { return pArray('2', kv...) }

func pArray(typ byte, kv ...string) xp {
	return func(w int) string {
		var entries []string
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i] == "" {
				entries = append(entries, fakeEmLong("", w))
				continue
			}
			entries = append(entries, fakeEmLong(kv[i], w)+fakeEmLong(kv[i+1], w))
		}
		return string(typ) + strings.Join(entries, "t") + "f"
	}
}

// gMsg is an RPC with chunk 2 version 0 and width 3.
func gMsg(name string, ps ...xp) []byte { return gMsgW(3, "0", name, ps...) }

func gMsgW(width int, version, name string, ps ...xp) []byte {
	body := ""
	for _, p := range ps {
		body += p(width)
	}
	return []byte(fmt.Sprintf("[XWB]11%d02", width) + fakeEmShort(version) + fakeEmShort(name) + "5" + body + "\x04")
}

// tcpConnect is TCPConnect with the given parameters.
func tcpConnect(ps ...xp) []byte {
	body := ""
	for _, p := range ps {
		body += p(3)
	}
	return []byte("[XWB]10304" + fakeEmShort("TCPConnect") + "5" + body + "\x04")
}

// --- grammar ---------------------------------------------------------------------

func TestVistaTCPParseTargets(t *testing.T) {
	legal := []struct{ target, address string }{
		{"tcp/10.10.218.1:9432", "10.10.218.1:9432"},
		{"tcp/127.0.0.1:1", "127.0.0.1:1"},
		{"tcp/203.0.113.5:65535", "203.0.113.5:65535"},
		{"tcp/[::1]:9430", "[::1]:9430"},
		{"tcp/[fd00::7]:9431", "[fd00::7]:9431"},
		{"tcp/[2001:db8::1]:9432", "[2001:db8::1]:9432"},
	}
	for _, c := range legal {
		r, err := parseVistaInput("vista /rpc " + c.target)
		if err != nil {
			t.Errorf("%s: %v", c.target, err)
			continue
		}
		l, err := r.limits()
		if err != nil {
			t.Fatalf("%s: %v", c.target, err)
		}
		if !l.tcp || l.network != "tcp" || l.address != c.address || l.tcpAddr.String() != c.address {
			t.Errorf("%s: %+v", c.target, l)
		}
		// A tcp route's own defaults.
		if l.signonTimeout != 170*time.Second || l.maxFailedSignons != 5 || l.failedSignonWindow != 15*time.Minute {
			t.Errorf("%s: defaults %v %d %v", c.target, l.signonTimeout, l.maxFailedSignons, l.failedSignonWindow)
		}
	}

	refused := []struct{ name, target, want string }{
		{"udp", "udp/10.0.0.1:9432", `network is "udp"`},
		{"tcp4", "tcp4/10.0.0.1:9432", `network is "tcp4"`},
		{"tcp6", "tcp6/[::1]:9432", `network is "tcp6"`},
		{"no network", "10.0.0.1:9432", "want unix/<socket path> or tcp/<IP>:<port>"},
		{"a name", "tcp/localhost:9432", "an IP address literal, not a name"},
		{"a dotted name", "tcp/broker.example.org:9432", "an IP address literal, not a name"},
		{"no host", "tcp/:9432", "an IP address literal, not a name"},
		{"a zone", "tcp/[fe80::1%en0]:9432", "carries no zone"},
		{"IPv4 unspecified", "tcp/0.0.0.0:9432", "unspecified address"},
		{"IPv6 unspecified", "tcp/[::]:9432", "unspecified address"},
		{"IPv4 multicast", "tcp/224.0.0.1:9432", "multicast"},
		{"IPv6 multicast", "tcp/[ff02::1]:9432", "multicast"},
		{"no port", "tcp/10.0.0.1", "a port from 1 through 65535"},
		{"port 0", "tcp/10.0.0.1:0", "a port from 1 through 65535"},
		{"port 65536", "tcp/10.0.0.1:65536", "out of range"},
		{"a port range", "tcp/10.0.0.1:9430-9432", "not a range"},
	}
	for _, c := range refused {
		_, err := parseVistaInput("vista /rpc " + c.target)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

func TestVistaTCPParseOptions(t *testing.T) {
	r, err := parseVistaInput("vista /rpc tcp/10.0.0.1:9432 {\n max_failed_signons 100\n failed_signon_window 24h\n signon_timeout 179s\n}")
	if err != nil {
		t.Fatal(err)
	}
	l, err := r.limits()
	if err != nil {
		t.Fatal(err)
	}
	if l.maxFailedSignons != 100 || l.failedSignonWindow != 24*time.Hour || l.signonTimeout != 179*time.Second {
		t.Fatalf("options: %+v", l)
	}
	r, err = parseVistaInput("vista /rpc tcp/10.0.0.1:9432 {\n max_failed_signons 1\n failed_signon_window 1m\n signon_timeout 10s\n}")
	if err != nil {
		t.Fatal(err)
	}
	if l, _ = r.limits(); l.maxFailedSignons != 1 || l.failedSignonWindow != time.Minute || l.signonTimeout != 10*time.Second {
		t.Fatalf("lowest options: %+v", l)
	}
	// A unix route keeps its own default and range, and has no failure limit.
	r, _ = parseVistaInput("vista /rpc unix//run/em.sock {\n signon_timeout 10m\n}")
	if l, err = r.limits(); err != nil || l.signonTimeout != 10*time.Minute || l.maxFailedSignons != 0 {
		t.Fatalf("unix route: %+v %v", l, err)
	}

	tcp := func(opts string) string { return "vista /rpc tcp/10.0.0.1:9432 {\n" + opts + "\n}" }
	unix := func(opts string) string { return "vista /rpc unix//run/em.sock {\n" + opts + "\n}" }
	cases := []struct{ name, input, want string }{
		{"max_failed_signons on unix", unix("max_failed_signons 5"), "max_failed_signons is for a tcp target only"},
		{"failed_signon_window on unix", unix("failed_signon_window 15m"), "failed_signon_window is for a tcp target only"},
		{"max_failed_signons 0", tcp("max_failed_signons 0"), "max_failed_signons: want 1 through 100"},
		{"max_failed_signons 101", tcp("max_failed_signons 101"), "max_failed_signons: want 1 through 100"},
		{"max_failed_signons word", tcp("max_failed_signons five"), "max_failed_signons: want 1 through 100"},
		{"failed_signon_window 59s", tcp("failed_signon_window 59s"), "failed_signon_window: want 1m0s through 24h0m0s"},
		{"failed_signon_window 25h", tcp("failed_signon_window 25h"), "failed_signon_window: want 1m0s through 24h0m0s"},
		{"signon_timeout 180s on tcp", tcp("signon_timeout 180s"), "signon_timeout 3m0s on a tcp target; want 10s through 2m59s"},
		{"signon_timeout 10m on tcp", tcp("signon_timeout 10m"), "on a tcp target"},
		{"signon_timeout 9s on tcp", tcp("signon_timeout 9s"), "signon_timeout: want 10s"},
		{"duplicate max_failed_signons", tcp("max_failed_signons 5\nmax_failed_signons 5"), `duplicate option "max_failed_signons"`},
	}
	for _, c := range cases {
		_, err := parseVistaInput(c.input)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// --- Start, the dial, status, and the session start line ----------------------

func TestVistaTCPStartWarnsOffPrivateNetworks(t *testing.T) {
	cases := []struct {
		target string
		warn   bool
	}{
		{"tcp/203.0.113.5:9432", true},
		{"tcp/[2001:db8::1]:9432", true},
		{"tcp/127.0.0.1:9432", false},
		{"tcp/[::1]:9432", false},
		{"tcp/10.10.218.1:9432", false},
		{"tcp/172.16.0.1:9432", false},
		{"tcp/192.168.1.1:9432", false},
		{"tcp/[fd00::1]:9432", false},
		{"unix//run/openvista/rpc.sock", false},
	}
	for _, c := range cases {
		core, logs := observer.New(zapcore.WarnLevel)
		a := &App{state: &janusState{vista: newVistaPool(zap.New(core))}, vistaKeys: map[string]bool{}}
		h := &Handler{logger: zap.NewNop(), Vista: []*VistaRoute{{Path: "/rpc", Target: c.target}}}
		if err := h.provisionVista(); err != nil {
			t.Fatal(err)
		}
		ha := &caddyhttp.App{Servers: map[string]*caddyhttp.Server{"s": vistaStartServer([]string{"vista.example.org"}, h, nil)}}
		if err := a.collectVista(ha); err != nil {
			t.Fatalf("%s: %v", c.target, err)
		}
		lines := logs.FilterMessageSnippet("neither loopback nor private").All()
		if got := len(lines) == 1; got != c.warn {
			t.Errorf("%s: %d warn lines, want warn=%v", c.target, len(lines), c.warn)
			continue
		}
		if c.warn && lines[0].ContextMap()["route"] != "vista.example.org/rpc" {
			t.Errorf("%s: the warn line names %v", c.target, lines[0].ContextMap())
		}
	}
}

func TestVistaTCPDialer(t *testing.T) {
	r, _ := parseVistaInput("vista /rpc tcp/10.0.0.1:9432")
	l, _ := r.limits()
	d := vistaDialer(l)
	if d.Timeout != 3*time.Second || !d.KeepAliveConfig.Enable || d.KeepAliveConfig.Idle != 15*time.Second ||
		d.KeepAliveConfig.Interval != 15*time.Second {
		t.Fatalf("tcp dialer: %+v", d)
	}
	r, _ = parseVistaInput("vista /rpc unix//run/em.sock")
	l, _ = r.limits()
	if d = vistaDialer(l); d.Timeout != 3*time.Second || d.KeepAliveConfig.Enable {
		t.Fatalf("unix dialer: %+v", d)
	}
}

// A session to a stock broker: no PROXY line (the fake fails the test on
// any byte before TCPConnect), accept, the pre-sign-on RPCs, sign-on with
// rotation, #BYE#; /1.0/vista shows the network and the guard's counters;
// the start line names the network and the dial's local port.
func TestVistaTCPSession(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	g := newVistaTCPRig(t, zap.New(core), nil)
	tok := g.connect()
	conn := g.em.Conns()[0]
	if raw := conn.Raw(); !bytes.HasPrefix(raw, xwbTCPConnectMsg()) {
		t.Fatalf("the broker's first bytes: %q", raw)
	}
	wantReply(t, "SIGNON SETUP", g.do(xwbRPCMsg("XUS SIGNON SETUP"), bearer(tok)...), emReplySignonSetup)
	wantReply(t, "INTRO MSG", g.do(xwbRPCMsg("XUS INTRO MSG"), bearer(tok)...), emReplyIntro)
	fresh := g.signon(tok)
	wantEnded(t, "the old token", g.do(imHereMsg, bearer(tok)...), http.StatusUnauthorized, vistaEndRotated)
	wantReply(t, "USERINFO", g.do(xwbRPCMsg("ORWU USERINFO"), bearer(fresh)...), emReplyUserInfo)
	wantReply(t, "#BYE#", g.do(xwbByeMsg(), bearer(fresh)...), emReplyBye)
	conn.WaitClosed(t)
	wantEnded(t, "after #BYE#", g.do(imHereMsg, bearer(fresh)...), http.StatusUnauthorized, vistaEndBye)
	if want := string(xwbTCPConnectMsg()) + string(xwbRPCMsg("XUS SIGNON SETUP")); !strings.HasPrefix(string(conn.Raw()), want) {
		t.Fatalf("the broker received %q", conn.Raw())
	}

	st := vistaStatusJSON(t, g.pool)
	var row map[string]any
	for _, r := range st {
		row = r
	}
	if row["network"] != "tcp" || row["target"] != "tcp/"+g.em.Addr() || row["failed_signons"] != 0.0 || row["failed_signon_holds"] != 0.0 {
		t.Fatalf("status: %v", row)
	}
	guard, _ := row["guard_refused"].(map[string]any)
	for _, rule := range []string{"command_params", "list_subscript", "literal_bound", "framing_byte", "signon_rpc", "reference", "signon_rerun"} {
		if guard[rule] != 0.0 {
			t.Fatalf("guard_refused %v lacks %s", guard, rule)
		}
	}
	if len(guard) != 7 {
		t.Fatalf("guard_refused %v", guard)
	}

	started := logs.FilterMessage("janus vista: session started").All()
	if len(started) != 1 {
		t.Fatalf("%d start lines", len(started))
	}
	f := started[0].ContextMap()
	if f["network"] != "tcp" || f["local_port"] != int64(conn.c.RemoteAddr().(*net.TCPAddr).Port) {
		t.Fatalf("start line %v; the broker saw port %d", f, conn.c.RemoteAddr().(*net.TCPAddr).Port)
	}
}

// A unix route is as it was: the PROXY line, no guard, network unix, no
// tcp counters, no local port.
func TestVistaUnixUnchangedByTCP(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	g := newVistaRig(t, zap.New(core), nil)
	tok := g.connect()
	if p := g.em.Conns()[0].Proxy(); !strings.HasPrefix(p, "PROXY TCP4 ") {
		t.Fatalf("PROXY line %q", p)
	}
	// em guards itself: what a tcp route refuses passes through.
	wantReply(t, "a $J subscript", g.do(gMsg("ORWU X", pList("$J", "v")), bearer(tok)...), emReplyEmpty)
	wantReply(t, "before sign-on", g.do(xwbRPCMsg("ORWU USERINFO"), bearer(tok)...), emReplyRefused)
	for _, r := range vistaStatusJSON(t, g.pool) {
		if r["network"] != "unix" {
			t.Fatalf("status: %v", r)
		}
		for _, k := range []string{"guard_refused", "failed_signons", "failed_signon_holds"} {
			if _, ok := r[k]; ok {
				t.Fatalf("a unix route shows %s: %v", k, r)
			}
		}
	}
	f := logs.FilterMessage("janus vista: session started").All()[0].ContextMap()
	if _, ok := f["local_port"]; ok || f["network"] != "unix" {
		t.Fatalf("unix start line %v", f)
	}
}

// --- the shape rules (1–4) -----------------------------------------------------

// shapeCase is one message judged at admission; rule "" means it passes
// to the broker, else the rule it breaks.
type shapeCase struct {
	name string
	msg  []byte
	rule string
}

func shapeCases() []shapeCase {
	q := func(n int) string { return `"` + strings.Repeat("x", n) + `"` }
	sub240 := q(200) + ",123456789012345,12345678901234,123456" // 4 levels, 240 bytes
	var cs []shapeCase
	ok := func(name string, msg []byte) { cs = append(cs, shapeCase{name, msg, ""}) }
	bad := func(name string, msg []byte, rule string) { cs = append(cs, shapeCase{name, msg, rule}) }
	sub := func(name, s, rule string) {
		cs = append(cs, shapeCase{"subscript " + name, gMsg("ORWU X", pList(s, "v")), rule})
	}

	// Rule 1: literals, as LIT^ZEMXWB reads them.
	sub(`"TEXT",1,0`, `"TEXT",1,0`, "")
	sub("1", "1", "")
	sub("-1.5", "-1.5", "")
	sub(".5", ".5", "")
	sub("-.5", "-.5", "")
	sub(`""""`, `""""`, "")
	sub(`"a""b",""`, `"a""b",""`, "")
	sub("$J", "$J", vistaGuardListSubscript)
	sub("X", "X", vistaGuardListSubscript)
	sub("1E3", "1E3", vistaGuardListSubscript)
	sub(`"a"_"b"`, `"a"_"b"`, vistaGuardListSubscript)
	sub("@X", "@X", vistaGuardListSubscript)
	sub("1,", "1,", vistaGuardListSubscript)
	sub(",1", ",1", vistaGuardListSubscript)
	sub("-", "-", vistaGuardListSubscript)
	sub("1.", "1.", vistaGuardListSubscript)
	sub("--1", "--1", vistaGuardListSubscript)
	sub(" 1", " 1", vistaGuardListSubscript)
	sub(`"open`, `"open`, vistaGuardListSubscript)
	sub(`$C(65)`, `$C(65)`, vistaGuardListSubscript)
	ok("an empty subscript mid-list", gMsg("ORWU X", pList("1", "a", "", "", "2", "b")))
	ok("an empty list", gMsg("ORWU X", pList("", "")))
	bad("a global array's subscript", gMsg("ORWU X", pArray('3', "$J", "v")), vistaGuardListSubscript)
	bad("a later entry's subscript", gMsg("ORWU X", pList("1", "a", "X", "b")), vistaGuardListSubscript)

	// Rule 2: bounds.
	sub("15 digits", "123456789012345", "")
	sub("15 digits with a fraction", "-1234567.89012345", "")
	sub("16 digits", "1234567890123456", vistaGuardLiteralBound)
	sub("16 digits with a fraction", "1234567890.123456", vistaGuardLiteralBound)
	sub("a 200-character string", q(200), "")
	sub("doubled quotes count once", `"`+strings.Repeat(`""`, 100)+`"`, "")
	sub("a 201-character string", q(201), vistaGuardLiteralBound)
	sub("8 levels", "1,2,3,4,5,6,7,8", "")
	sub("9 levels", "1,2,3,4,5,6,7,8,9", vistaGuardLiteralBound)
	sub("240 bytes", sub240, "")
	sub("241 bytes", sub240+"7", vistaGuardLiteralBound)
	ok("a 32,000-byte value", gMsgW(9, "0", "ORWU X", pList("1", strings.Repeat("v", 32000))))
	bad("a 32,001-byte value", gMsgW(9, "0", "ORWU X", pList("1", strings.Repeat("v", 32001))), vistaGuardLiteralBound)
	ok("a 32,000-byte literal", gMsgW(9, "0", "ORWU X", pLit(strings.Repeat("v", 32000))))
	bad("a 32,001-byte literal", gMsgW(9, "0", "ORWU X", pLit(strings.Repeat("v", 32001))), vistaGuardLiteralBound)

	// Rule 3: no framing bytes in any field.
	for _, fb := range []struct{ name, b string }{{`\x04`, "\x04"}, {"[XWB]", "[XWB]"}} {
		bad(fb.name+" in chunk 1", []byte("[XWB]11301"+fakeEmShort("1"+fb.b)+fakeEmShort("")+"2"+fakeEmShort("0")+fakeEmShort("ORWU X")+"5\x04"), vistaGuardFramingByte)
		bad(fb.name+" in the RPC's version", gMsgW(3, "0"+fb.b, "ORWU X"), vistaGuardFramingByte)
		bad(fb.name+" in the RPC's name", gMsg("ORWU"+fb.b+"X"), vistaGuardFramingByte)
		bad(fb.name+" in a literal", gMsg("ORWU X", pLit("a"+fb.b+"b")), vistaGuardFramingByte)
		bad(fb.name+" in a reference", gMsg("ORWU X", pRef("X"+fb.b)), vistaGuardFramingByte)
		bad(fb.name+" in a subscript", gMsg("ORWU X", pList(`"`+fb.b+`"`, "v")), vistaGuardFramingByte)
		bad(fb.name+" in a value", gMsg("ORWU X", pList("1", "a"+fb.b)), vistaGuardFramingByte)
		bad(fb.name+" in TCPConnect's name", tcpConnect(pLit("127.0.0.1"), pLit("0"), pLit("cp"+fb.b+"rs")), vistaGuardFramingByte)
	}
	ok("[XWB without its bracket", gMsg("ORWU X", pLit("[XWB")))
	ok("a 4-character name, whose length byte is \\x04", gMsg("MERX"))
	return cs
}

func tcpConnectCases() []shapeCase {
	lit := pLit
	return []shapeCase{
		{"TCPConnect as CPRS sends it", xwbTCPConnectMsg(), ""},
		{"TCPConnect with an empty name", tcpConnect(lit("127.0.0.1"), lit("0"), lit("")), ""},
		{"TCPConnect with two literals", tcpConnect(lit("127.0.0.1"), lit("0")), vistaGuardCommandParams},
		{"TCPConnect with four literals", tcpConnect(lit("127.0.0.1"), lit("0"), lit("cprs"), lit("x")), vistaGuardCommandParams},
		{"TCPConnect with no parameters", []byte("[XWB]10304" + fakeEmShort("TCPConnect") + "\x04"), vistaGuardCommandParams},
		{"TCPConnect with an empty chunk 5", []byte("[XWB]10304" + fakeEmShort("TCPConnect") + "5\x04"), vistaGuardCommandParams},
		{"TCPConnect with ^ in the name", tcpConnect(lit("127.0.0.1"), lit("0"), lit("cp^rs")), vistaGuardCommandParams},
		{"TCPConnect with ^ in the address", tcpConnect(lit("127.0.0.1^x"), lit("0"), lit("cprs")), vistaGuardCommandParams},
		{"TCPConnect with a port not all digits", tcpConnect(lit("127.0.0.1"), lit("9x"), lit("cprs")), vistaGuardCommandParams},
		{"TCPConnect with an empty port", tcpConnect(lit("127.0.0.1"), lit(""), lit("cprs")), vistaGuardCommandParams},
		{"TCPConnect with a reference", tcpConnect(lit("127.0.0.1"), lit("0"), pRef("X")), vistaGuardCommandParams},
		{"TCPConnect with a list", tcpConnect(lit("127.0.0.1"), lit("0"), pList("1", "a")), vistaGuardCommandParams},
		{"TCPConnect with an empty parameter", tcpConnect(lit("127.0.0.1"), lit("0"), func(int) string { return "4f" }), vistaGuardCommandParams},
		{"TCPConnect with a bad list subscript", tcpConnect(lit("127.0.0.1"), lit("0"), pList("$J", "a")), vistaGuardListSubscript},
	}
}

// Every shape case on a signed-on session: a refused message gets the
// fixed reply, never reaches the broker, and the session goes on; an
// accepted one reaches it. No log line carries a value.
func TestVistaTCPGuardShapeRules(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	g := newVistaTCPRig(t, zap.New(core), nil)
	tok := g.signon(g.connect())
	conn := g.em.Conns()[0]
	want := map[string]int64{}
	var refused [][]byte
	for _, c := range shapeCases() {
		before := g.received()
		r := g.do(c.msg, bearer(tok)...)
		if c.rule == "" {
			wantReply(t, c.name, r, emReplyEmpty)
			if g.received() != before+1 {
				t.Fatalf("%s: the broker did not receive it", c.name)
			}
			continue
		}
		wantReply(t, c.name, r, tcpReplyInvalid)
		if r.h.Get("Content-Type") != "application/x-xwb" || vistaSetToken(r.h) != "" {
			t.Fatalf("%s: headers %v", c.name, r.h)
		}
		if g.received() != before {
			t.Fatalf("%s: the broker received it", c.name)
		}
		want[c.rule]++
		refused = append(refused, c.msg)
		// The session goes on, its stream untouched.
		wantReply(t, c.name+": then IM HERE", g.do(imHereMsg, bearer(tok)...), emReplyIMHere)
	}
	raw := conn.Raw()
	for _, m := range refused {
		if bytes.Contains(raw, m) {
			t.Fatalf("the broker's bytes hold a refused message %q", m[:min(len(m), 40)])
		}
	}
	// #BYE# carries no chunk 5; the session goes on, then ends by a proper one.
	wantReply(t, "#BYE# with chunk 5", g.do([]byte("[XWB]10304\x05#BYE#5\x04"), bearer(tok)...), tcpReplyInvalid)
	want[vistaGuardCommandParams]++
	wantReply(t, "after it", g.do(imHereMsg, bearer(tok)...), emReplyIMHere)

	st := g.status()
	for _, rule := range vistaGuardRules {
		if st.GuardRefused[rule] != want[rule] {
			t.Errorf("guard_refused %s = %d, want %d", rule, st.GuardRefused[rule], want[rule])
		}
	}
	if st.Requests != int64(g.received()-1) { // TCPConnect is not a request
		t.Errorf("requests %d counts refused messages (the broker received %d)", st.Requests, g.received())
	}
	for _, e := range logs.All() {
		text := e.Message
		for k, v := range e.ContextMap() {
			text += " " + k + "=" + fmt.Sprint(v)
		}
		for _, s := range []string{"ORWU", "xxxxxxxx", "$J", "1E3", "TCPConnect"} {
			if strings.Contains(text, s) {
				t.Errorf("log line %q carries %q", text, s)
			}
		}
	}
}

// TCPConnect's shape is judged before a session exists: a refused one
// opens no connection and replaces no session.
func TestVistaTCPGuardTCPConnect(t *testing.T) {
	g := newVistaTCPRig(t, nil, nil)
	live := g.connect()
	for _, c := range tcpConnectCases() {
		conns := len(g.em.Conns())
		r := g.do(c.msg, bearer(live)...)
		if c.rule == "" {
			wantReply(t, c.name, r, emReplyAccept)
			g.em.WaitConns(t, conns+1)
			live = vistaSetToken(r.h)
			continue
		}
		wantReply(t, c.name, r, tcpReplyInvalid)
		if len(g.em.Conns()) != conns {
			t.Fatalf("%s: a connection was opened", c.name)
		}
		wantReply(t, c.name+": the session lives", g.do(imHereMsg, bearer(live)...), emReplyIMHere)
	}
	if st := g.status(); st.GuardRefused[vistaGuardCommandParams] != 11 || st.GuardRefused[vistaGuardListSubscript] != 1 {
		t.Fatalf("guard_refused %v", st.GuardRefused)
	}
}

// --- the session rules (5–7) ----------------------------------------------------

func TestVistaTCPGuardSessionRules(t *testing.T) {
	g := newVistaTCPRig(t, nil, nil)
	tok := g.connect()
	pass := func(what string, msg []byte, tok string) {
		t.Helper()
		before := g.received()
		r := g.do(msg, bearer(tok)...)
		if r.code != http.StatusOK || bytes.Equal(r.body, tcpReplyUnauthorized) || g.received() != before+1 {
			t.Fatalf("%s: %d %q, received %d→%d", what, r.code, r.body, before, g.received())
		}
	}
	refuse := func(what string, msg []byte, tok string) {
		t.Helper()
		before := g.received()
		wantReply(t, what, g.do(msg, bearer(tok)...), tcpReplyUnauthorized)
		if g.received() != before {
			t.Fatalf("%s: the broker received it", what)
		}
	}

	// Rule 5: before sign-on only the sign-on RPCs, byte-exact.
	for _, rpc := range []string{"XUS SIGNON SETUP", "XUS INTRO MSG", "XUS AV CODE", "XUS AV HELP", "XUS CVC",
		"XUS SET SHARED", "XWB GET BROKER INFO", "XWB IM HERE", "XWB IS RPC AVAILABLE"} {
		pass(rpc+" before sign-on", xwbRPCMsg(rpc), tok)
	}
	for _, rpc := range []string{"ORWU USERINFO", "XWB CREATE CONTEXT", "XUS ESSO VALIDATE", "XUS AV CODE ", "xus av code", "XUS AV CODE^X"} {
		refuse(rpc+" before sign-on", xwbRPCMsg(rpc), tok)
	}
	// Rule 6: no reference before sign-on, even on a sign-on RPC.
	refuse("a reference before sign-on", gMsg("XUS AV CODE", pRef("DUZ")), tok)

	tok = g.signon(tok)
	pass("USERINFO after sign-on", xwbRPCMsg("ORWU USERINFO"), tok)
	pass("INTRO MSG after sign-on", xwbRPCMsg("XUS INTRO MSG"), tok)
	// Rule 6 after sign-on: a local variable, any subscripts literals.
	for _, v := range []string{"DUZ", "DUZ(1)", `%ZX("a",2)`, "X1", "%", `ORY("TEXT",1,0)`, "A(-.5)"} {
		pass("reference "+v, gMsg("ORWU X", pRef(v)), tok)
	}
	for _, v := range []string{"$J", "@X", "^X", "^X(1)", "X(", "X()", "X(1", "DUZ(1E3)", "DUZ(1)(2)", "1X", "X Y", "X_Y",
		"", "X($J)", `X("a"_"b")`, "X(1234567890123456)", "X(" + strings.Repeat("1,", 8) + "9)"} {
		refuse("reference "+v, gMsg("ORWU X", pRef(v)), tok)
	}
	// Rule 7: no second sign-on.
	for _, rpc := range []string{"XUS SIGNON SETUP", "XUS AV CODE", "XUS CVC", "XUS ESSO VALIDATE"} {
		refuse(rpc+" after sign-on", xwbRPCMsg(rpc), tok)
	}
	st := g.status()
	if st.GuardRefused[vistaGuardSignonRPC] != 6 || st.GuardRefused[vistaGuardReference] != 18 || st.GuardRefused[vistaGuardSignonRerun] != 4 {
		t.Fatalf("guard_refused %v", st.GuardRefused)
	}
	wantReply(t, "the session goes on", g.do(imHereMsg, bearer(tok)...), emReplyIMHere)
}

// The session rules judge a request when it leaves the queue: one queued
// behind XUS AV CODE sees the state that sign-on left.
func TestVistaTCPGuardQueuedBehindSignon(t *testing.T) {
	for _, tc := range []struct {
		code   string
		passes bool
	}{{" GOOD;CODE ", true}, {" BAD;CODE ", false}} {
		t.Run(strings.TrimSpace(tc.code), func(t *testing.T) {
			g := newVistaTCPRig(t, nil, nil)
			g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
				if m.RPC == "XUS AV CODE" {
					return append([]fakeEmAction{{Delay: 300 * time.Millisecond}}, fakeEmDefault(c, m)...)
				}
				return fakeEmDefault(c, m)
			})
			tok := g.connect()
			conn := g.em.Conns()[0]
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				g.do(xwbRPCMsg("XUS AV CODE", tc.code), bearer(tok)...)
			}()
			vistaWait(t, "the sign-on to reach the broker", func() bool { return len(conn.Received()) == 2 })
			r := g.do(xwbRPCMsg("ORWU USERINFO"), bearer(tok)...)
			wg.Wait()
			if tc.passes {
				wantReply(t, "queued behind a sign-on", r, emReplyUserInfo)
			} else {
				wantReply(t, "queued behind a failed sign-on", r, tcpReplyUnauthorized)
				if len(conn.Received()) != 2 {
					t.Fatalf("the broker received %d messages", len(conn.Received()))
				}
			}
		})
	}
}

// --- failed sign-ons -------------------------------------------------------------

func TestVistaFailedSignonsCounter(t *testing.T) {
	var f vistaFailedSignons
	a := netip.MustParsePrefix("192.0.2.7/32")
	b := netip.MustParsePrefix("2001:db8:1::/64")
	t0 := time.Unix(1_800_000_000, 0)
	w := 15 * time.Minute
	// The limit: the fifth failure in the window begins a hold.
	for i := range 4 {
		if f.fail(a, 5, w, t0.Add(time.Duration(i)*time.Minute)) || f.held(a, t0.Add(time.Duration(i)*time.Minute)) {
			t.Fatalf("held after %d failures", i+1)
		}
	}
	if !f.fail(a, 5, w, t0.Add(4*time.Minute)) {
		t.Fatal("the fifth failure began no hold")
	}
	// Per key: another key is not held.
	if f.held(b, t0.Add(5*time.Minute)) {
		t.Fatal("another key is held")
	}
	// The hold lasts one window from its start, then the key starts afresh.
	if !f.held(a, t0.Add(4*time.Minute+w-time.Second)) {
		t.Fatal("the hold ended early")
	}
	if f.held(a, t0.Add(4*time.Minute+w)) {
		t.Fatal("the hold outlasted its window")
	}
	if f.fail(a, 5, w, t0.Add(20*time.Minute)) {
		t.Fatal("a key that starts afresh is held at once")
	}
	// The window: failures older than it no longer count.
	for i := range 4 {
		f.fail(b, 5, w, t0.Add(time.Duration(i)*time.Minute))
	}
	if f.fail(b, 5, w, t0.Add(w+time.Minute)) || f.held(b, t0.Add(w+time.Minute)) {
		t.Fatal("failures outside the window counted")
	}
	// A large map sweeps keys whose window and hold have passed.
	for i := range vistaFailedSignonsSweep + 1 {
		k := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)}), 32)
		f.fail(k, 5, w, t0)
	}
	f.fail(a, 5, w, t0.Add(48*time.Hour))
	if len(f.keys) != 1 {
		t.Fatalf("%d keys after a sweep", len(f.keys))
	}
}

func TestVistaTCPFailedSignonLimit(t *testing.T) {
	g := newVistaTCPRig(t, nil, func(l *vistaLimits) {
		l.maxFailedSignons = 3
		l.failedSignonWindow = 600 * time.Millisecond
	})
	tok := g.connect()
	avs := func() int {
		n := 0
		for _, c := range g.em.Conns() {
			for _, m := range c.Received() {
				if m.RPC == "XUS AV CODE" {
					n++
				}
			}
		}
		return n
	}
	// An expired verify code is no failure.
	wantReply(t, "expired", g.do(xwbRPCMsg("XUS AV CODE", " EXPIRED;CODE "), bearer(tok)...), emReplyAVExpired)
	for i := range 3 {
		wantReply(t, fmt.Sprintf("failure %d", i+1), g.do(xwbRPCMsg("XUS AV CODE", " BAD;CODE "), bearer(tok)...), emReplyAVBad)
	}
	// The hold: even the right code is refused, never sent; other RPCs run.
	wantReply(t, "held", g.do(xwbRPCMsg("XUS AV CODE", " GOOD;CODE "), bearer(tok)...), tcpReplyHeld)
	wantReply(t, "IM HERE while held", g.do(imHereMsg, bearer(tok)...), emReplyIMHere)
	// The hold is the client key's, not the session's.
	other := g.connect()
	wantReply(t, "held on another session", g.do(xwbRPCMsg("XUS AV CODE", " GOOD;CODE "), bearer(other)...), tcpReplyHeld)
	if n := avs(); n != 4 {
		t.Fatalf("the broker received %d XUS AV CODE, want 4", n)
	}
	st := g.status()
	if *st.FailedSignons != 3 || *st.FailedSignonHolds != 2 || st.SignedOn != 0 {
		t.Fatalf("failed_signons %d, holds %d, signed_on %d", *st.FailedSignons, *st.FailedSignonHolds, st.SignedOn)
	}
	// The hold ends with its window.
	time.Sleep(700 * time.Millisecond)
	g.signon(tok)
	if st := g.status(); st.SignedOn != 1 || *st.FailedSignons != 3 {
		t.Fatalf("after the hold: %+v", st)
	}
}

func TestVistaTCPFailedSignonWindow(t *testing.T) {
	g := newVistaTCPRig(t, nil, func(l *vistaLimits) {
		l.maxFailedSignons = 3
		l.failedSignonWindow = 400 * time.Millisecond
	})
	tok := g.connect()
	bad := xwbRPCMsg("XUS AV CODE", " BAD;CODE ")
	g.do(bad, bearer(tok)...)
	g.do(bad, bearer(tok)...)
	time.Sleep(500 * time.Millisecond)
	wantReply(t, "a failure after the window", g.do(bad, bearer(tok)...), emReplyAVBad)
	wantReply(t, "and another", g.do(bad, bearer(tok)...), emReplyAVBad)
	g.signon(tok)
	if st := g.status(); *st.FailedSignonHolds != 0 || *st.FailedSignons != 4 {
		t.Fatalf("holds %d failures %d", *st.FailedSignonHolds, *st.FailedSignons)
	}
}

// Failures count per client key: one client held, another signs on.
func TestVistaTCPFailedSignonPerKey(t *testing.T) {
	g := newVistaTCPRig(t, nil, func(l *vistaLimits) { l.maxFailedSignons = 2 })
	post := func(remote string, body []byte, tok string) ([]byte, string) {
		r := vistaDirectReq(http.MethodPost, body, bearer(tok)...)
		r.RemoteAddr = remote
		rr, _ := vistaServeDirect(g.h, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %q", remote, rr.Code, rr.Body.Bytes())
		}
		return rr.Body.Bytes(), vistaSetToken(rr.Header())
	}
	_, a := post("192.0.2.7:4000", xwbTCPConnectMsg(), "")
	_, b := post("[2001:db8:1::5]:4000", xwbTCPConnectMsg(), "")
	bad := xwbRPCMsg("XUS AV CODE", " BAD;CODE ")
	good := xwbRPCMsg("XUS AV CODE", " GOOD;CODE ")
	post("192.0.2.7:4001", bad, a)
	post("192.0.2.7:4002", bad, a)
	if got, _ := post("192.0.2.7:4003", good, a); !bytes.Equal(got, tcpReplyHeld) {
		t.Fatalf("client a: %q", got)
	}
	// The same /64 is the same key; another prefix is not.
	if got, fresh := post("[2001:db8:1::5]:4000", good, b); !bytes.Equal(got, emReplyAVGood) || fresh == "" {
		t.Fatalf("client b: %q", got)
	}
}

// --- timeouts and the broker's own endings ---------------------------------------

// The broker's own pre-sign-on read ends at 180 s with TIME-OUT; Janus's
// signon_timeout ends the session first, so the TIME-OUT never arrives.
func TestVistaTCPSignonTimeoutBeforeTheBrokers(t *testing.T) {
	g := newVistaTCPRig(t, nil, func(l *vistaLimits) { l.signonTimeout = 200 * time.Millisecond })
	g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		if m.Command == "TCPConnect" {
			time.AfterFunc(800*time.Millisecond, func() { c.Send([]byte("\x00\x00TIME-OUT\x04")); c.Close() })
		}
		return fakeEmDefault(c, m)
	})
	start := time.Now()
	tok := g.connect()
	g.em.Conns()[0].WaitClosed(t)
	if took := time.Since(start); took > 700*time.Millisecond {
		t.Fatalf("the session ended after %v, not before the broker's TIME-OUT", took)
	}
	wantEnded(t, "after the deadline", g.do(imHereMsg, bearer(tok)...), http.StatusUnauthorized, vistaEndSignonTimeout)
	if st := g.status(); st.Ended["signon_timeout"] != 1 || st.Ended["protocol"] != 0 {
		t.Fatalf("ended %v", st.Ended)
	}
}

// After sign-on the broker's activity timeout sends #BYE# unasked and
// exits: the session ends closed, whole or split, by EOF or half-close.
func TestVistaTCPBrokerByeWhileIdle(t *testing.T) {
	cases := []struct {
		name string
		act  func(c *fakeEmConn)
	}{
		{"#BYE# then close", func(c *fakeEmConn) { c.Send(emReplyBye); c.Close() }},
		{"#BYE# split, then close", func(c *fakeEmConn) {
			c.Send(emReplyBye[:4])
			time.Sleep(50 * time.Millisecond)
			c.Send(emReplyBye[4:])
			time.Sleep(50 * time.Millisecond)
			c.Close()
		}},
		{"#BYE# then a half-close", func(c *fakeEmConn) { c.Send(emReplyBye); c.CloseWrite() }},
		{"a half-close alone", func(c *fakeEmConn) { c.CloseWrite() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newVistaTCPRig(t, nil, nil)
			tok := g.signon(g.connect())
			tc.act(g.em.Conns()[0])
			vistaWait(t, "the session to end", func() bool { return g.status().Sessions == 0 })
			wantEnded(t, "after the broker's end", g.do(imHereMsg, bearer(tok)...), http.StatusUnauthorized, vistaEndClosed)
			if st := g.status(); st.Ended["closed"] != 1 || st.Ended["protocol"] != 0 {
				t.Fatalf("ended %v", st.Ended)
			}
		})
	}

	// A request that meets the broker's #BYE# before its EOF: closed, and
	// the request never gets the #BYE# as its reply.
	t.Run("a request after #BYE#", func(t *testing.T) {
		g := newVistaTCPRig(t, nil, nil)
		tok := g.signon(g.connect())
		conn := g.em.Conns()[0]
		conn.Send(emReplyBye)
		time.Sleep(100 * time.Millisecond)
		r := g.do(imHereMsg, bearer(tok)...)
		wantEnded(t, "the request", r, http.StatusBadGateway, vistaEndClosed)
	})

	// Anything else unasked while idle is still protocol: the broker's
	// TIME-OUT, or bytes after a #BYE#.
	for name, b := range map[string][]byte{
		"TIME-OUT":           []byte("\x00\x00TIME-OUT\x04"),
		"bytes after #BYE#":  append(append([]byte(nil), emReplyBye...), 'x'),
		"a #BYE# lookalike":  []byte("\x00\x00#BYX#\x04"),
		"an unasked empty":   emReplyEmpty,
		"a security message": tcpReplyInvalid,
	} {
		t.Run(name, func(t *testing.T) {
			g := newVistaTCPRig(t, nil, nil)
			tok := g.signon(g.connect())
			g.em.Conns()[0].Send(b)
			vistaWait(t, "the session to end", func() bool { return g.status().Sessions == 0 })
			wantEnded(t, "after unasked bytes", g.do(imHereMsg, bearer(tok)...), http.StatusUnauthorized, vistaEndProtocol)
		})
	}
}

// A result holding \x04 ends its reply early; the session ends protocol at
// the reply's end when the rest is buffered, else on the next read. Either
// way no request gets the rest as its reply.
func TestVistaTCPEarlyEndByteInData(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("split=%v", split), func(t *testing.T) {
			g := newVistaTCPRig(t, nil, nil)
			g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
				if m.RPC == "ORWU X" {
					if split {
						return []fakeEmAction{{Write: []byte("\x00\x00ab\x04")}, {Delay: 100 * time.Millisecond}, {Write: []byte("cd\x04")}}
					}
					return fakeEmWrite([]byte("\x00\x00ab\x04cd\x04"))
				}
				return fakeEmDefault(c, m)
			})
			tok := g.signon(g.connect())
			wantReply(t, "the early reply", g.do(gMsg("ORWU X"), bearer(tok)...), []byte("\x00\x00ab\x04"))
			vistaWait(t, "the session to end", func() bool { return g.status().Sessions == 0 })
			wantEnded(t, "the next request", g.do(imHereMsg, bearer(tok)...), http.StatusUnauthorized, vistaEndProtocol)
		})
	}
}
