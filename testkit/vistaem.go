package main

// Acceptance fixture for capability 11 (vista): a fake em. In production a
// service manager accepts on a Unix socket and starts one em process per
// connection running VistA's broker loop, which requires a PROXY v1 line
// first, then reads XWB messages and writes exactly one reply each
// (OpenVista's docs/RPC-HTTPS.md §2–§3). This serves the same, with
// RPC-HTTPS §3's measured replies, until killed. The Go tests' in-package
// fake (vista_fakeem_test.go) has the same default policy.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	vemAccept      = []byte("\x00\x00accept\x04")
	vemReject      = []byte("\x00\x00reject\x04")
	vemBye         = []byte("\x00\x00#BYE#\x04")
	vemIMHere      = []byte("\x00\x001\x04")
	vemBrokerInfo  = []byte("\x00\x00180\r\n\x04")
	vemSignonSetup = []byte("\x00\x00pop\r\nENV\r\nENV\r\n/dev/null\r\n5\r\n0\r\nFOIA.DOMAIN.EXT\r\n0\r\n\x04")
	vemIntro       = []byte("\x00\x00Welcome to OpenVista.\r\nAuthorized users only.\r\n\x04")
	vemAVGood      = []byte("\x00\x00100\r\n0\r\n0\r\n\r\n0\r\n0\r\n\r\nGood evening MANAGER,SITE\r\n\x04")
	vemAVBad       = []byte("\x00\x000\r\n0\r\n0\r\n\r\n0\r\n0\r\n\r\nNot a valid ACCESS CODE/VERIFY CODE pair.\r\n\x04")
	vemAVExpired   = []byte("\x00\x000\r\n0\r\n1\r\n\r\n0\r\n0\r\n\r\nVERIFY CODE must be changed before continued use.\r\n\x04")
	vemCVC         = []byte("\x00\x000\r\nThe verify code has been changed.\r\n\x04")
	vemUserInfo    = []byte("\x00\x00100^MANAGER,SITE^0^0^0^0^0^300^20^1^1^5^FOIA.DOMAIN.EXT^0^180\x04")
	vemRefused     = []byte("\x3aRemote Procedure 'ORWU USERINFO' cannot run before sign-on\x00\x04")
	vemMErr        = []byte("\x00\x00\x18M  ERROR=CAPI+16^XWBPRS,4 actual parameters for IMHERE^XWBLIB,M58,-M58\r\nLAST REF=^XTV(8989.3,1,\"XWB\")\x04\x04")
	vemEmpty       = []byte("\x00\x00\x04")
)

// cmdVistaEm: `testkit vistaem --sock S [--log F]` — a fake em on a unix
// socket. Beyond RPC-HTTPS §3's replies: TCPConnect named "reject" gets
// reject and the stream's end; MERR an M error; ECHO its first literal;
// BIG N bytes of data; CLOSE-AFTER MS an empty reply, then the stream's
// end MS milliseconds later (em's idle timeout). The log holds one line
// per connection and message, never a parameter's value.
func cmdVistaEm(args []string) {
	var sock, logf string
	flagValues(args, map[string]*string{"--sock": &sock, "--log": &logf})
	if sock == "" {
		die("usage: testkit vistaem --sock S [--log F]")
	}
	logLine := func(format string, a ...any) {
		if logf != "" {
			appendLine(logf, fmt.Sprintf(format, a...))
		}
	}
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		die("testkit: listen %s: %v", sock, err)
	}
	var n atomic.Int64
	for {
		c, err := l.Accept()
		if err != nil {
			die("testkit: accept %s: %v", sock, err)
		}
		go vemServe(c, int(n.Add(1)), logLine)
	}
}

// vemConn is one stream: one em process
type vemConn struct {
	c        net.Conn
	once     sync.Once
	signedOn bool
	changing bool
}

func (v *vemConn) close() { v.once.Do(func() { v.c.Close() }) }

func vemServe(c net.Conn, id int, logLine func(string, ...any)) {
	v := &vemConn{c: c}
	defer v.close()
	r := bufio.NewReader(c)
	line, err := vemReadProxy(r)
	if err != nil {
		logLine("conn %d bad-proxy=%q", id, line)
		return
	}
	logLine("conn %d proxy=%s", id, line)
	defer logLine("end %d", id)
	for {
		m, err := vemReadMsg(r)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				logLine("conn %d refused", id)
			}
			return
		}
		logLine("msg %d rpc=%q cmd=%q", id, m.rpc, m.cmd)
		writes, closeAfter := v.reply(m)
		for _, w := range writes {
			if _, err := c.Write(w); err != nil {
				return
			}
		}
		switch {
		case closeAfter == 0:
			return
		case closeAfter > 0:
			time.AfterFunc(closeAfter, v.close)
		}
	}
}

// reply is a message's reply in em's writes, and when to end the stream
// after it: never (-1), now (0), or later
func (v *vemConn) reply(m vemMsg) ([][]byte, time.Duration) {
	one := func(b []byte) ([][]byte, time.Duration) { return [][]byte{b}, -1 }
	switch m.cmd {
	case "TCPConnect":
		if m.param(2) == "reject" {
			return [][]byte{vemReject}, 0
		}
		return one(vemAccept)
	case "#BYE#":
		return [][]byte{vemBye}, 0
	}
	switch m.rpc {
	case "XWB IM HERE":
		return one(vemIMHere)
	case "XWB GET BROKER INFO":
		return one(vemBrokerInfo)
	case "XUS SIGNON SETUP":
		return one(vemSignonSetup)
	case "XUS INTRO MSG":
		return one(vemIntro)
	case "XUS AV CODE":
		code := strings.TrimSuffix(strings.TrimPrefix(m.param(0), " "), " ")
		switch code {
		case "GOOD;CODE":
			v.signedOn = true
			return one(vemAVGood)
		case "EXPIRED;CODE":
			v.changing = true
			return one(vemAVExpired)
		}
		return one(vemAVBad)
	case "XUS CVC":
		if v.changing {
			v.signedOn = true
		}
		return one(vemCVC)
	case "ORWU USERINFO":
		if v.signedOn {
			return one(vemUserInfo)
		}
		return one(vemRefused)
	case "MERR":
		return one(vemMErr)
	case "ECHO":
		return one(append(append([]byte("\x00\x00"), m.param(0)...), 4))
	case "BIG":
		n, _ := strconv.Atoi(m.param(0))
		b := make([]byte, 0, n+3)
		b = append(b, 0, 0)
		for i := 0; i < n; i++ {
			b = append(b, 'a'+byte(i%26))
		}
		b = append(b, 4)
		var pieces [][]byte
		for len(b) > 32000 {
			pieces = append(pieces, b[:32000])
			b = b[32000:]
		}
		return append(pieces, b), -1
	case "CLOSE-AFTER":
		ms, err := strconv.Atoi(m.param(0))
		if err != nil || ms < 0 {
			ms = 0
		}
		return [][]byte{vemEmpty}, time.Duration(ms)*time.Millisecond + 1
	}
	return one(vemEmpty)
}

// vemReadProxy reads a PROXY v1 line (at most 107 bytes with its \r\n):
// TCP4 or TCP6, two addresses of that family, two ports
func vemReadProxy(r *bufio.Reader) (string, error) {
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
		return string(line), errors.New("no \\r\\n")
	}
	s := string(line[:len(line)-2])
	f := strings.Split(s, " ")
	if len(f) != 6 || f[0] != "PROXY" || (f[1] != "TCP4" && f[1] != "TCP6") {
		return s, errors.New("not a PROXY line")
	}
	for _, a := range f[2:4] {
		ip, err := netip.ParseAddr(a)
		if err != nil || ip.Zone() != "" || ip.Is4() != (f[1] == "TCP4") {
			return s, errors.New("bad address")
		}
	}
	for _, p := range f[4:6] {
		if n, err := strconv.Atoi(p); err != nil || n < 0 || n > 65535 || strconv.Itoa(n) != p {
			return s, errors.New("bad port")
		}
	}
	return s, nil
}

type vemMsg struct {
	cmd, rpc string
	params   []string // literal (type 0) values
}

func (m vemMsg) param(i int) string {
	if i < len(m.params) {
		return m.params[i]
	}
	return ""
}

// vemReadMsg reads exactly one broker message, walking its structure as
// VistA's parser does (OpenVista's relay/xwb.go)
func vemReadMsg(r *bufio.Reader) (vemMsg, error) {
	w := &vemWalker{r: r}
	var m vemMsg
	head, err := w.take(5)
	if err != nil {
		return m, err
	}
	switch string(head) {
	case "#BYE#":
		m.cmd = "#BYE#"
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
		return m, errors.New("the header asks for no reply")
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
				m.rpc = string(name)
			}
		case '3':
		case '4':
			var cmd []byte
			cmd, err = w.short()
			m.cmd = string(cmd)
		case '5':
			m.params, err = w.params(width)
			if err == nil {
				return m, nil
			}
		default: // the end of the chunks, \x04
			return m, nil
		}
		if err != nil {
			return m, err
		}
	}
}

type vemWalker struct{ r *bufio.Reader }

func (w *vemWalker) take(n int) ([]byte, error) {
	buf := make([]byte, n)
	_, err := io.ReadFull(w.r, buf)
	return buf, err
}

func (w *vemWalker) byte() (byte, error) { return w.r.ReadByte() }

func (w *vemWalker) short() ([]byte, error) {
	n, err := w.byte()
	if err != nil {
		return nil, err
	}
	return w.take(int(n))
}

func (w *vemWalker) long(width int) ([]byte, error) {
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
// literal values in order
func (w *vemWalker) params(width int) ([]string, error) {
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
