package janus

// The fake em: a test double for the vista capability's backend. In
// production a service manager accepts on a Unix socket and starts one em
// process per connection running VistA's broker loop (XWBP^%ZISTCPS),
// which requires a PROXY v1 line first, then reads XWB messages and writes
// exactly one reply each (RPC-HTTPS §2–§3). This fake does the same with
// scripted replies whose defaults are RPC-HTTPS §3's measured bytes.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Replies, byte for byte (RPC-HTTPS §3)
var (
	emReplyAccept      = []byte("\x00\x00accept\x04")
	emReplyReject      = []byte("\x00\x00reject\x04")
	emReplyBye         = []byte("\x00\x00#BYE#\x04")
	emReplyIMHere      = []byte("\x00\x001\x04")
	emReplyBrokerInfo  = []byte("\x00\x00180\r\n\x04")
	emReplySignonSetup = []byte("\x00\x00pop\r\nENV\r\nENV\r\n/dev/null\r\n5\r\n0\r\nFOIA.DOMAIN.EXT\r\n0\r\n\x04")
	emReplyIntro       = []byte("\x00\x00Welcome to OpenVista.\r\nAuthorized users only.\r\n\x04")
	emReplyAVGood      = []byte("\x00\x00100\r\n0\r\n0\r\n\r\n0\r\n0\r\n\r\nGood evening MANAGER,SITE\r\n\x04")
	emReplyAVBad       = []byte("\x00\x000\r\n0\r\n0\r\n\r\n0\r\n0\r\n\r\nNot a valid ACCESS CODE/VERIFY CODE pair.\r\n\x04")
	emReplyAVExpired   = []byte("\x00\x000\r\n0\r\n1\r\n\r\n0\r\n0\r\n\r\nVERIFY CODE must be changed before continued use.\r\n\x04")
	emReplyCVC         = []byte("\x00\x000\r\nThe verify code has been changed.\r\n\x04")
	emReplyUserInfo    = []byte("\x00\x00100^MANAGER,SITE^0^0^0^0^0^300^20^1^1^5^FOIA.DOMAIN.EXT^0^180\x04")
	emReplyRefused     = []byte("\x3aRemote Procedure 'ORWU USERINFO' cannot run before sign-on\x00\x04")
	emReplyMErr        = []byte("\x00\x00\x18M  ERROR=CAPI+16^XWBPRS,4 actual parameters for IMHERE^XWBLIB,M58,-M58\r\nLAST REF=^XTV(8989.3,1,\"XWB\")\x04\x04")
	emReplyEmpty       = []byte("\x00\x00\x04")
)

// fakeEmPiece is the size of em's writes (OpenVista's XWBRW edit)
const fakeEmPiece = 32000

// fakeEm serves a Unix socket like em behind a service manager: one
// goroutine per connection, the PROXY line first, then one scripted reply
// per message.
type fakeEm struct {
	path string
	ln   net.Listener

	mu     sync.Mutex
	script func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction
	conns  []*fakeEmConn
}

// fakeEmMsg is one broker message as the fake read it
type fakeEmMsg struct {
	Raw          []byte
	Command, RPC string
	Params       []string // literal param values, type 0
}

// fakeEmConn is one accepted stream: one em process
type fakeEmConn struct {
	em  *fakeEm
	c   net.Conn
	r   *bufio.Reader
	wmu sync.Mutex

	mu       sync.Mutex
	proxy    string
	received []fakeEmMsg
	closed   bool
	signedOn bool // the default policy's sign-on state
	changing bool // the default policy's expired-verify-code state
}

// fakeEmAction is one step of a reply: Write bytes, sleep Delay, Close the
// stream, or Stall (never answer; block until the conn closes). Each set
// field runs in that order.
type fakeEmAction struct {
	Write []byte
	Delay time.Duration
	Close bool
	Stall bool
}

func newFakeEm(t *testing.T) *fakeEm {
	t.Helper()
	dir, err := os.MkdirTemp("", "em")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	e := &fakeEm{path: path, ln: ln, script: fakeEmDefault}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fc := &fakeEmConn{em: e, c: c, r: bufio.NewReader(c)}
			e.mu.Lock()
			e.conns = append(e.conns, fc)
			e.mu.Unlock()
			go fc.serve()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
		for _, c := range e.Conns() {
			c.Close()
		}
		os.RemoveAll(dir)
	})
	return e
}

// Path is the socket's path
func (e *fakeEm) Path() string { return e.path }

// Script replaces the reply policy; fakeEmDefault is the default, and a
// script may fall through to it
func (e *fakeEm) Script(fn func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.script = fn
}

// Conns is every connection accepted so far, in order
func (e *fakeEm) Conns() []*fakeEmConn {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*fakeEmConn(nil), e.conns...)
}

// WaitConns waits up to 5 s until n connections were accepted
func (e *fakeEm) WaitConns(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := len(e.Conns()); got >= n {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("fake em: %d connections accepted, want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Proxy is the PROXY line received, without its \r\n (whatever arrived
// first, when it was not a valid PROXY line)
func (c *fakeEmConn) Proxy() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.proxy
}

// Received is the messages received, in order
func (c *fakeEmConn) Received() []fakeEmMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]fakeEmMsg(nil), c.received...)
}

// Closed reports whether the stream has ended, by either side
func (c *fakeEmConn) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// WaitClosed waits up to 5 s until the stream has ended
func (c *fakeEmConn) WaitClosed(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !c.Closed() {
		if time.Now().After(deadline) {
			t.Fatal("fake em: the stream did not end")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// SignedOn reports the default policy's sign-on state
func (c *fakeEmConn) SignedOn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.signedOn
}

// Close ends the stream, as em does at its idle timeout or a crash
func (c *fakeEmConn) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.c.Close()
}

// Send writes unsolicited bytes now
func (c *fakeEmConn) Send(b []byte) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.c.Write(b); err != nil {
		c.Close()
	}
}

func (c *fakeEmConn) serve() {
	defer c.Close()
	line, err := fakeEmReadProxy(c.r)
	c.mu.Lock()
	c.proxy = line
	c.mu.Unlock()
	if err != nil {
		return // em ends a stream that does not start with a PROXY line
	}
	for {
		m, err := fakeEmReadMsg(c.r)
		if err != nil {
			return // EOF, a closed conn, or a message em's parser refuses
		}
		c.mu.Lock()
		c.received = append(c.received, m)
		c.mu.Unlock()
		c.em.mu.Lock()
		script := c.em.script
		c.em.mu.Unlock()
		for _, a := range script(c, m) {
			if a.Write != nil {
				c.wmu.Lock()
				_, err := c.c.Write(a.Write)
				c.wmu.Unlock()
				if err != nil {
					return
				}
			}
			if a.Delay > 0 {
				time.Sleep(a.Delay)
			}
			if a.Close {
				return
			}
			if a.Stall {
				// never answer: discard what arrives until the stream ends
				_, _ = io.Copy(io.Discard, c.r)
				return
			}
		}
	}
}

// fakeEmReadProxy reads a PROXY v1 line (at most 107 bytes with its
// \r\n) and checks it as XWBP^%ZISTCPS needs it: TCP4 or TCP6, two
// addresses of that family, two ports
func fakeEmReadProxy(r *bufio.Reader) (string, error) {
	var line []byte
	for len(line) < 107 {
		b, err := r.ReadByte()
		if err != nil {
			return string(line), err
		}
		line = append(line, b)
		if b == '\n' {
			break
		}
	}
	if !bytes.HasSuffix(line, []byte("\r\n")) {
		return string(line), errors.New("the PROXY line does not end with \\r\\n")
	}
	s := string(line[:len(line)-2])
	f := strings.Split(s, " ")
	if len(f) != 6 || f[0] != "PROXY" || (f[1] != "TCP4" && f[1] != "TCP6") {
		return s, fmt.Errorf("not a PROXY line: %q", s)
	}
	for _, a := range f[2:4] {
		ip, err := netip.ParseAddr(a)
		if err != nil || ip.Zone() != "" || ip.Is4() != (f[1] == "TCP4") {
			return s, fmt.Errorf("a PROXY address is %q", a)
		}
	}
	for _, p := range f[4:6] {
		if n, err := strconv.Atoi(p); err != nil || n < 0 || n > 65535 || strconv.Itoa(n) != p {
			return s, fmt.Errorf("a PROXY port is %q", p)
		}
	}
	return s, nil
}

// fakeEmReadMsg reads exactly one broker message, walking its structure as
// VistA's parser does (openvista relay/xwb.go's readMessage), and keeps its
// literal parameters
func fakeEmReadMsg(r *bufio.Reader) (fakeEmMsg, error) {
	w := &fakeEmWalker{r: r}
	head, err := w.take(5)
	if err != nil {
		return fakeEmMsg{}, err
	}
	m := fakeEmMsg{}
	switch string(head) {
	case "#BYE#":
		m.Command, m.Raw = "#BYE#", w.got
		return m, nil
	case "[XWB]":
	default:
		return m, errors.New("not a broker message")
	}
	digits, err := w.take(4)
	if err != nil {
		return m, err
	}
	for _, d := range digits {
		if d < '0' || d > '9' {
			return m, fmt.Errorf("the header's digits are %q", digits)
		}
	}
	if digits[3] != '0' {
		return m, fmt.Errorf("the header asks for no reply (%q)", digits)
	}
	width := int(digits[2] - '0')
	if width == 0 {
		width = 3
	}
	for {
		chunk, err := w.byte()
		if err != nil {
			return m, err
		}
		switch chunk {
		case '1':
			if _, err = w.short(); err == nil {
				_, err = w.short()
			}
		case '2':
			if _, err = w.short(); err == nil {
				var name []byte
				name, err = w.short()
				m.RPC = string(name)
			}
		case '3':
		case '4':
			var cmd []byte
			cmd, err = w.short()
			m.Command = string(cmd)
		case '5':
			m.Params, err = w.params(width)
			if err == nil {
				m.Raw = w.got
				return m, nil
			}
		default: // the end of the chunks, \x04
			m.Raw = w.got
			return m, nil
		}
		if err != nil {
			return m, err
		}
	}
}

type fakeEmWalker struct {
	r   *bufio.Reader
	got []byte
}

func (w *fakeEmWalker) take(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(w.r, buf); err != nil {
		return nil, err
	}
	w.got = append(w.got, buf...)
	return buf, nil
}

func (w *fakeEmWalker) byte() (byte, error) {
	b, err := w.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (w *fakeEmWalker) short() ([]byte, error) {
	n, err := w.byte()
	if err != nil {
		return nil, err
	}
	return w.take(int(n))
}

func (w *fakeEmWalker) long(width int) ([]byte, error) {
	digits, err := w.take(width)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, d := range digits {
		if d < '0' || d > '9' {
			return nil, fmt.Errorf("a length is %q", digits)
		}
		n = n*10 + int(d-'0')
	}
	return w.take(n)
}

// params walks the parameters chunk to its end byte (PRS5), returning the
// literal (type 0) values in order
func (w *fakeEmWalker) params(width int) ([]string, error) {
	var lits []string
	cont := byte('f')
	var typ byte
	for {
		if cont == 'f' {
			t, err := w.byte()
			if err != nil {
				return nil, err
			}
			typ = t
		}
		switch typ {
		case 4:
			return lits, nil
		case '0', '1':
			v, err := w.long(width)
			if err != nil {
				return nil, err
			}
			if typ == '0' {
				lits = append(lits, string(v))
			}
		case '2', '3':
			sub, err := w.long(width)
			if err != nil {
				return nil, err
			}
			if len(sub) > 0 {
				if _, err := w.long(width); err != nil {
					return nil, err
				}
			}
		case '4', '5':
		default:
			return nil, fmt.Errorf("a parameter's type is %q", typ)
		}
		c, err := w.byte()
		if err != nil {
			return nil, err
		}
		cont = c
		if cont != 't' {
			cont = 'f'
		}
	}
}

// fakeEmWrite is one action writing b
func fakeEmWrite(b []byte) []fakeEmAction { return []fakeEmAction{{Write: b}} }

// fakeEmBigData is BIG's data of n bytes: a fixed pattern without \x04
func fakeEmBigData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a' + byte(i%26)
	}
	return b
}

// fakeEmBigReply is BIG's whole reply for n bytes of data
func fakeEmBigReply(n int) []byte {
	return append(append([]byte("\x00\x00"), fakeEmBigData(n)...), 4)
}

// fakeEmPieces splits a reply into em's 32,000-byte writes
func fakeEmPieces(b []byte) []fakeEmAction {
	var out []fakeEmAction
	for len(b) > fakeEmPiece {
		out = append(out, fakeEmAction{Write: b[:fakeEmPiece]})
		b = b[fakeEmPiece:]
	}
	return append(out, fakeEmAction{Write: b})
}

// fakeEmCode trims one leading and one trailing space, as a code sent
// wrapped in spaces arrives (RPC-HTTPS §4)
func fakeEmCode(s string) string {
	s = strings.TrimPrefix(s, " ")
	return strings.TrimSuffix(s, " ")
}

func fakeEmParam(m fakeEmMsg, i int) string {
	if i < len(m.Params) {
		return m.Params[i]
	}
	return ""
}

// fakeEmDefault is the default reply policy, from RPC-HTTPS §3's measured
// replies. TCPConnect whose third literal (the client's name) is "reject"
// gets reject and the stream's end.
func fakeEmDefault(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
	switch m.Command {
	case "TCPConnect":
		if fakeEmParam(m, 2) == "reject" {
			return []fakeEmAction{{Write: emReplyReject, Close: true}}
		}
		return fakeEmWrite(emReplyAccept)
	case "#BYE#":
		return []fakeEmAction{{Write: emReplyBye, Close: true}}
	}
	switch m.RPC {
	case "XWB IM HERE":
		return fakeEmWrite(emReplyIMHere)
	case "XWB GET BROKER INFO":
		return fakeEmWrite(emReplyBrokerInfo)
	case "XUS SIGNON SETUP":
		return fakeEmWrite(emReplySignonSetup)
	case "XUS INTRO MSG":
		return fakeEmWrite(emReplyIntro)
	case "XUS AV CODE":
		c.mu.Lock()
		defer c.mu.Unlock()
		switch fakeEmCode(fakeEmParam(m, 0)) {
		case "GOOD;CODE":
			c.signedOn = true
			return fakeEmWrite(emReplyAVGood)
		case "EXPIRED;CODE":
			c.changing = true
			return fakeEmWrite(emReplyAVExpired)
		}
		return fakeEmWrite(emReplyAVBad)
	case "XUS CVC":
		c.mu.Lock()
		if c.changing {
			c.signedOn = true
		}
		c.mu.Unlock()
		return fakeEmWrite(emReplyCVC)
	case "ORWU USERINFO":
		if c.SignedOn() {
			return fakeEmWrite(emReplyUserInfo)
		}
		return fakeEmWrite(emReplyRefused)
	case "MERR":
		return fakeEmWrite(emReplyMErr)
	case "ECHO":
		return fakeEmWrite(append(append([]byte("\x00\x00"), fakeEmParam(m, 0)...), 4))
	case "BIG":
		n, _ := strconv.Atoi(fakeEmParam(m, 0))
		return fakeEmPieces(fakeEmBigReply(n))
	}
	return fakeEmWrite(emReplyEmpty)
}

// Request messages, encoded as CPRS sends them (openvista relay_test.go)

func fakeEmShort(s string) string { return string([]byte{byte(len(s))}) + s }

func fakeEmLong(s string, width int) string {
	return fmt.Sprintf("%0*d", width, len(s)) + s
}

// xwbRPCMsg is an RPC with literal parameters; a literal over 999 bytes
// switches the message to 9-digit lengths
func xwbRPCMsg(name string, literals ...string) []byte {
	width := 3
	for _, l := range literals {
		if len(l) > 999 {
			width = 9
		}
	}
	body := ""
	for _, l := range literals {
		body += "0" + fakeEmLong(l, width) + "f"
	}
	return []byte("[XWB]11" + strconv.Itoa(width) + "02" + fakeEmShort("1.108") + fakeEmShort(name) + "5" + body + "\x04")
}

// xwbTCPConnectMsg is TCPConnect from 127.0.0.1, named cprs
func xwbTCPConnectMsg() []byte { return xwbTCPConnectNamedMsg("cprs") }

// xwbTCPConnectNamedMsg is TCPConnect with the given client name; the
// default policy rejects the name "reject"
func xwbTCPConnectNamedMsg(name string) []byte {
	return []byte("[XWB]10304" + fakeEmShort("TCPConnect") + "5" +
		"0" + fakeEmLong("127.0.0.1", 3) + "f" + "0" + fakeEmLong("0", 3) + "f" + "0" + fakeEmLong(name, 3) + "f" + "\x04")
}

// xwbByeMsg is the #BYE# command, as CPRS sends it
func xwbByeMsg() []byte { return []byte("[XWB]10304\x05#BYE#\x04") }
