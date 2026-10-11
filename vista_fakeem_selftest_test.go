package janus

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeEmDial connects to the fake em and writes the PROXY line
func fakeEmDial(t *testing.T, e *fakeEm, proxy string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("unix", e.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := io.WriteString(c, proxy); err != nil {
		t.Fatal(err)
	}
	return c, bufio.NewReader(c)
}

// fakeEmReadReply reads one reply by RPC-HTTPS §3's rule
func fakeEmReadReply(t *testing.T, c net.Conn, r *bufio.Reader) []byte {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer c.SetReadDeadline(time.Time{})
	var out []byte
	for range 2 {
		n, err := r.ReadByte()
		if err != nil {
			t.Fatal(err)
		}
		s := make([]byte, n)
		if _, err := io.ReadFull(r, s); err != nil {
			t.Fatal(err)
		}
		out = append(append(out, n), s...)
	}
	data, err := r.ReadBytes(4)
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, data...)
	if len(data) > 1 && data[0] == 0x18 {
		b, err := r.ReadByte()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func fakeEmExchange(t *testing.T, c net.Conn, r *bufio.Reader, msg []byte, want []byte) {
	t.Helper()
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	if got := fakeEmReadReply(t, c, r); !bytes.Equal(got, want) {
		t.Fatalf("reply %q, want %q", got, want)
	}
}

// fakeEmWaitEOF expects the stream to end with nothing more on it (EOF,
// or a reset when em closed with bytes it never read)
func fakeEmWaitEOF(t *testing.T, c net.Conn, r *bufio.Reader) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := r.ReadByte()
	if ne, ok := err.(net.Error); err == nil || ok && ne.Timeout() {
		t.Fatalf("read %q, %v; want the stream's end", b, err)
	}
}

const fakeEmProxy4 = "PROXY TCP4 192.0.2.7 127.0.0.1 51234 443\r\n"

func TestFakeEmSignOnAndBye(t *testing.T) {
	e := newFakeEm(t)
	c, r := fakeEmDial(t, e, fakeEmProxy4)
	fakeEmExchange(t, c, r, xwbTCPConnectMsg(), emReplyAccept)
	fakeEmExchange(t, c, r, xwbRPCMsg("XUS SIGNON SETUP"), emReplySignonSetup)
	fakeEmExchange(t, c, r, xwbRPCMsg("XWB GET BROKER INFO"), emReplyBrokerInfo)
	fakeEmExchange(t, c, r, xwbRPCMsg("XWB IM HERE"), []byte("\x00\x001\x04"))
	fakeEmExchange(t, c, r, xwbRPCMsg("ORWU USERINFO"), emReplyRefused)
	if emReplyRefused[0] != 0x3a {
		t.Fatalf("the refusal's length byte is %#x", emReplyRefused[0])
	}
	fakeEmExchange(t, c, r, xwbRPCMsg("XUS AV CODE", " BAD;CODE "), emReplyAVBad)
	fakeEmExchange(t, c, r, xwbRPCMsg("XUS AV CODE", " GOOD;CODE "), emReplyAVGood)
	fakeEmExchange(t, c, r, xwbRPCMsg("ORWU USERINFO"), emReplyUserInfo)
	fakeEmExchange(t, c, r, xwbRPCMsg("UNKNOWN RPC"), emReplyEmpty)
	fakeEmExchange(t, c, r, xwbByeMsg(), emReplyBye)
	fakeEmWaitEOF(t, c, r)

	e.WaitConns(t, 1)
	ec := e.Conns()[0]
	ec.WaitClosed(t)
	if ec.Proxy() != strings.TrimSuffix(fakeEmProxy4, "\r\n") {
		t.Fatalf("proxy %q", ec.Proxy())
	}
	got := ec.Received()
	if len(got) != 10 {
		t.Fatalf("%d messages received", len(got))
	}
	if got[0].Command != "TCPConnect" || !bytes.Equal(got[0].Raw, xwbTCPConnectMsg()) ||
		strings.Join(got[0].Params, ",") != "127.0.0.1,0,cprs" {
		t.Fatalf("TCPConnect read as %+v", got[0])
	}
	if got[6].RPC != "XUS AV CODE" || len(got[6].Params) != 1 || got[6].Params[0] != " GOOD;CODE " {
		t.Fatalf("XUS AV CODE read as %+v", got[6])
	}
	if got[9].Command != "#BYE#" || !bytes.Equal(got[9].Raw, xwbByeMsg()) {
		t.Fatalf("#BYE# read as %+v", got[9])
	}
}

func TestFakeEmExpiredThenCVC(t *testing.T) {
	e := newFakeEm(t)
	c, r := fakeEmDial(t, e, "PROXY TCP6 2001:db8::1 ::1 0 443\r\n")
	fakeEmExchange(t, c, r, xwbTCPConnectMsg(), emReplyAccept)
	fakeEmExchange(t, c, r, xwbRPCMsg("XUS AV CODE", " EXPIRED;CODE "), emReplyAVExpired)
	if !bytes.HasPrefix(emReplyAVExpired, []byte("\x00\x000\r\n0\r\n1\r\n")) {
		t.Fatal("the expired fixture's third line is not 1")
	}
	fakeEmExchange(t, c, r, xwbRPCMsg("ORWU USERINFO"), emReplyRefused)
	fakeEmExchange(t, c, r, xwbRPCMsg("XUS CVC", " OLD ", " NEW ", " NEW "), emReplyCVC)
	fakeEmExchange(t, c, r, xwbRPCMsg("ORWU USERINFO"), emReplyUserInfo)
	e.WaitConns(t, 1)
	if !e.Conns()[0].SignedOn() {
		t.Fatal("not signed on after XUS CVC")
	}
}

func TestFakeEmRejectEndsTheStream(t *testing.T) {
	e := newFakeEm(t)
	c, r := fakeEmDial(t, e, fakeEmProxy4)
	fakeEmExchange(t, c, r, xwbTCPConnectNamedMsg("reject"), emReplyReject)
	fakeEmWaitEOF(t, c, r)
}

func TestFakeEmMErrorEchoAndBig(t *testing.T) {
	e := newFakeEm(t)
	c, r := fakeEmDial(t, e, fakeEmProxy4)
	fakeEmExchange(t, c, r, xwbRPCMsg("MERR"), emReplyMErr)
	if !bytes.HasSuffix(emReplyMErr, []byte("\x04\x04")) || emReplyMErr[2] != 0x18 {
		t.Fatal("the M error fixture lacks \\x18 or its second \\x04")
	}
	fakeEmExchange(t, c, r, xwbRPCMsg("ECHO", "hello"), []byte("\x00\x00hello\x04"))
	long := strings.Repeat("z", 1500) // 9-digit lengths
	fakeEmExchange(t, c, r, xwbRPCMsg("ECHO", long), []byte("\x00\x00"+long+"\x04"))
	fakeEmExchange(t, c, r, xwbRPCMsg("BIG", "100000"), fakeEmBigReply(100000))
	fakeEmExchange(t, c, r, xwbRPCMsg("XWB IM HERE"), emReplyIMHere)
}

func TestFakeEmScriptSplitsDelaysAndCloses(t *testing.T) {
	e := newFakeEm(t)
	e.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		if m.RPC == "SPLIT" {
			return []fakeEmAction{
				{Write: emReplyAVGood[:3]},
				{Delay: 20 * time.Millisecond},
				{Write: emReplyAVGood[3:]},
			}
		}
		if m.RPC == "HALF" {
			return []fakeEmAction{{Write: emReplyAVGood[:5], Close: true}}
		}
		return fakeEmDefault(c, m)
	})
	c, r := fakeEmDial(t, e, fakeEmProxy4)
	fakeEmExchange(t, c, r, xwbTCPConnectMsg(), emReplyAccept)
	fakeEmExchange(t, c, r, xwbRPCMsg("SPLIT"), emReplyAVGood)
	if _, err := c.Write(xwbRPCMsg("HALF")); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, emReplyAVGood[:5]) {
		t.Fatalf("read %q, %v", got, err)
	}
	e.Conns()[0].WaitClosed(t)
}

func TestFakeEmStallCloseAndSend(t *testing.T) {
	e := newFakeEm(t)
	e.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		if m.RPC == "STALL" {
			return []fakeEmAction{{Stall: true}}
		}
		return fakeEmDefault(c, m)
	})

	// Stall: no answer, until the peer closes
	c1, r1 := fakeEmDial(t, e, fakeEmProxy4)
	if _, err := c1.Write(xwbRPCMsg("STALL")); err != nil {
		t.Fatal(err)
	}
	c1.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if b, err := r1.ReadByte(); err == nil {
		t.Fatalf("a stalled RPC answered %q", b)
	}
	e.WaitConns(t, 1)
	stalled := e.Conns()[0]
	if stalled.Closed() {
		t.Fatal("the stalled conn ended early")
	}
	c1.Close()
	stalled.WaitClosed(t)

	// Close: em ends an idle stream
	c2, r2 := fakeEmDial(t, e, fakeEmProxy4)
	fakeEmExchange(t, c2, r2, xwbTCPConnectMsg(), emReplyAccept)
	e.WaitConns(t, 2)
	e.Conns()[1].Close()
	fakeEmWaitEOF(t, c2, r2)

	// Send: unsolicited bytes arrive while no request runs
	c3, r3 := fakeEmDial(t, e, fakeEmProxy4)
	fakeEmExchange(t, c3, r3, xwbTCPConnectMsg(), emReplyAccept)
	e.WaitConns(t, 3)
	e.Conns()[2].Send([]byte("junk"))
	c3.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, 4)
	if _, err := io.ReadFull(r3, got); err != nil || string(got) != "junk" {
		t.Fatalf("read %q, %v", got, err)
	}
}

func TestFakeEmRefusesABadProxyLine(t *testing.T) {
	for _, line := range []string{
		"[XWB]10304\x0aTCPConnect5",
		"PROXY TCP4 192.0.2.7 127.0.0.1 51234\r\n",
		"PROXY TCP4 2001:db8::1 127.0.0.1 1 443\r\n",
		"PROXY UNKNOWN\r\n",
		"PROXY TCP4 192.0.2.7 127.0.0.1 51234 443\n",
	} {
		e := newFakeEm(t)
		c, r := fakeEmDial(t, e, line)
		c.Write(xwbTCPConnectMsg())
		fakeEmWaitEOF(t, c, r)
		e.WaitConns(t, 1)
		ec := e.Conns()[0]
		ec.WaitClosed(t)
		if len(ec.Received()) != 0 || ec.Proxy() == "" {
			t.Fatalf("%q: proxy %q, received %d", line, ec.Proxy(), len(ec.Received()))
		}
	}
}
