package janus

import (
	"bytes"
	"fmt"
	"net/netip"
	"strconv"
)

// xwbEnd is the byte that ends a request and a reply; xwbMError begins the
// data of a reply that is an M error.
const (
	xwbEnd    = 0x04
	xwbMError = 0x18
)

// xwbMessage is what Janus needs of one request: its type and name.
type xwbMessage struct {
	Command string // type 0: "TCPConnect" or "#BYE#"; "" for an RPC
	RPC     string // type 1: the RPC name from chunk 2; "" for a command
}

// xwbwWalk walks a request in place: every value is a slice of the body,
// never a copy, and every declared length is checked before it is taken.
type xwbwWalk struct {
	b []byte
	i int
}

func (w *xwbwWalk) early() error {
	return fmt.Errorf("xwb: the message ends early at byte %d", w.i)
}

func (w *xwbwWalk) byte() (byte, error) {
	if w.i >= len(w.b) {
		return 0, w.early()
	}
	c := w.b[w.i]
	w.i++
	return c, nil
}

func (w *xwbwWalk) take(n int, what string) ([]byte, error) {
	if left := len(w.b) - w.i; n > left {
		return nil, fmt.Errorf("xwb: %s at byte %d declares %d bytes; %d remain", what, w.i, n, left)
	}
	s := w.b[w.i : w.i+n]
	w.i += n
	return s, nil
}

// shortString takes one length byte and that many bytes.
func (w *xwbwWalk) shortString() ([]byte, error) {
	n, err := w.byte()
	if err != nil {
		return nil, err
	}
	return w.take(int(n), "a short string")
}

// longString takes width decimal digits and that many bytes.
func (w *xwbwWalk) longString(width int) ([]byte, error) {
	at := w.i
	digits, err := w.take(width, "a long string's length")
	if err != nil {
		return nil, err
	}
	n := 0
	for _, d := range digits {
		if d < '0' || d > '9' {
			return nil, fmt.Errorf("xwb: a long string's length at byte %d is not %d digits", at, width)
		}
		n = n*10 + int(d-'0')
	}
	return w.take(n, "a long string")
}

// params walks chunk 5 through the end byte: each parameter is a type byte
// and its value closed by f; a list's entries are joined by t, and an entry
// with an empty subscript has no value.
func (w *xwbwWalk) params(width int) error {
	for {
		at := w.i
		typ, err := w.byte()
		if err != nil {
			return err
		}
		switch typ {
		case xwbEnd:
			return nil
		case '0', '1':
			if _, err := w.longString(width); err != nil {
				return err
			}
		case '2', '3':
			for {
				sub, err := w.longString(width)
				if err != nil {
					return err
				}
				if len(sub) > 0 {
					if _, err := w.longString(width); err != nil {
						return err
					}
				}
				c, err := w.byte()
				if err != nil {
					return err
				}
				if c == 'f' {
					break
				}
				if c != 't' {
					return fmt.Errorf("xwb: a list entry at byte %d is followed by neither t nor f", at)
				}
			}
			continue
		case '4', '5':
		default:
			return fmt.Errorf("xwb: the parameter at byte %d has no known type", at)
		}
		c, err := w.byte()
		if err != nil {
			return err
		}
		if c != 'f' {
			return fmt.Errorf("xwb: the parameter at byte %d is not closed by f", at)
		}
	}
}

// walkXWB checks that body is exactly one well-formed XWB message per the
// contract's walker rules and returns what it is. Errors are descriptive
// and never quote the body.
func walkXWB(body []byte) (xwbMessage, error) {
	var m xwbMessage
	if len(body) < 5 || string(body[:5]) != "[XWB]" {
		return m, fmt.Errorf("xwb: the message does not start with [XWB]")
	}
	w := xwbwWalk{b: body, i: 5}
	head, err := w.take(4, "the header")
	if err != nil {
		return m, err
	}
	if head[0] != '1' {
		return m, fmt.Errorf("xwb: the header's version is not 1")
	}
	typ := head[1]
	if typ != '0' && typ != '1' {
		return m, fmt.Errorf("xwb: the header's type is neither 0 nor 1")
	}
	if head[2] < '0' || head[2] > '9' {
		return m, fmt.Errorf("xwb: the header's width is not a digit")
	}
	width := int(head[2] - '0')
	if width == 0 {
		width = 3
	}
	if head[3] != '0' {
		return m, fmt.Errorf("xwb: the header's fourth digit is not 0")
	}

	var last byte = '0'
	var name, command []byte
	var hasName, hasCommand bool
	for done := false; !done; {
		at := w.i
		c, err := w.byte()
		if err != nil {
			return m, err
		}
		if c == xwbEnd {
			break
		}
		if c < '1' || c > '5' {
			return m, fmt.Errorf("xwb: byte %d is neither a chunk id nor the end byte", at)
		}
		if c <= last {
			return m, fmt.Errorf("xwb: chunk %c at byte %d is out of order or repeated", c, at)
		}
		last = c
		switch c {
		case '1':
			if _, err = w.shortString(); err == nil {
				_, err = w.shortString()
			}
		case '2':
			if typ != '1' {
				return m, fmt.Errorf("xwb: a command carries chunk 2")
			}
			if _, err = w.shortString(); err == nil {
				name, err = w.shortString()
				hasName = true
			}
		case '3':
		case '4':
			if typ != '0' {
				return m, fmt.Errorf("xwb: an RPC carries chunk 4")
			}
			command, err = w.shortString()
			hasCommand = true
		case '5':
			err = w.params(width)
			done = true
		}
		if err != nil {
			return m, err
		}
	}
	if w.i != len(body) {
		return m, fmt.Errorf("xwb: %d bytes follow the end byte", len(body)-w.i)
	}

	if typ == '1' {
		if !hasName {
			return m, fmt.Errorf("xwb: an RPC lacks chunk 2")
		}
		if len(name) == 0 {
			return m, fmt.Errorf("xwb: an RPC's name is empty")
		}
		m.RPC = string(name)
		return m, nil
	}
	if !hasCommand {
		return m, fmt.Errorf("xwb: a command lacks chunk 4")
	}
	switch string(command) {
	case "TCPConnect":
		m.Command = "TCPConnect"
	case "#BYE#":
		m.Command = "#BYE#"
	default:
		return m, fmt.Errorf("xwb: the command is neither TCPConnect nor #BYE#")
	}
	return m, nil
}

// xwbReplyScanner states, in the order a reply passes through them.
const (
	xwbsLen1 uint8 = iota
	xwbsStr1
	xwbsLen2
	xwbsStr2
	xwbsFirst
	xwbsData
	xwbsMEnd
	xwbsDone
)

// xwbsPrefixMax is how much of a reply's data the scanner keeps.
const xwbsPrefixMax = 64

// xwbReplyScanner finds the end of one XWB reply in a byte stream, fed
// incrementally (RPC-HTTPS §3: short string, short string, data to \x04,
// then one more \x04 when the data's first byte is \x18). It never searches
// past the reply's end and never waits for the stream to pause.
type xwbReplyScanner struct {
	state  uint8
	need   int // bytes left in the current short string
	dirty  bool
	mErr   bool
	total  int64
	prefix [xwbsPrefixMax]byte
	plen   int
}

// Feed consumes from p up to the reply's end. n is how many leading bytes
// of p belong to this reply (n < len(p) only when done; the rest is not
// the reply's and the caller treats it as leftover). done reports the reply
// ended. An M error's data whose \x04 is followed by any byte but \x04 ends
// the reply before that byte, so the byte is leftover. After done, Feed
// consumes nothing.
func (s *xwbReplyScanner) Feed(p []byte) (n int, done bool) {
	for n < len(p) && s.state != xwbsDone {
		switch s.state {
		case xwbsLen1, xwbsLen2:
			l := int(p[n])
			n++
			if l != 0 {
				s.dirty = true
			}
			s.need = l
			s.state++
			if l == 0 {
				s.state++
			}
		case xwbsStr1, xwbsStr2:
			k := min(s.need, len(p)-n)
			n += k
			s.need -= k
			if s.need == 0 {
				s.state++
			}
		case xwbsFirst:
			if p[n] == xwbEnd {
				n++
				s.state = xwbsDone
				break
			}
			s.mErr = p[n] == xwbMError
			s.state = xwbsData
		case xwbsData:
			rest := p[n:]
			i := bytes.IndexByte(rest, xwbEnd)
			if i >= 0 {
				rest = rest[:i]
			}
			s.plen += copy(s.prefix[s.plen:], rest)
			n += len(rest)
			if i >= 0 {
				n++
				if s.mErr {
					s.state = xwbsMEnd
				} else {
					s.state = xwbsDone
				}
			}
		case xwbsMEnd:
			if p[n] == xwbEnd {
				n++
			}
			s.state = xwbsDone
		}
	}
	s.total += int64(n)
	return n, s.state == xwbsDone
}

// Total is how many bytes of the reply have been consumed.
func (s *xwbReplyScanner) Total() int64 { return s.total }

// Clean reports that no short string so far is non-empty; once PastShorts,
// that both were empty.
func (s *xwbReplyScanner) Clean() bool { return !s.dirty }

// MError reports that the data began with \x18.
func (s *xwbReplyScanner) MError() bool { return s.mErr }

// DataPrefix is the first ≤64 bytes of data seen so far, held by the
// scanner; the end byte is not data.
func (s *xwbReplyScanner) DataPrefix() []byte { return s.prefix[:s.plen] }

// PastShorts reports that both short strings have been consumed.
func (s *xwbReplyScanner) PastShorts() bool { return s.state >= xwbsFirst }

type signonVerdict int

const (
	signonNone    signonVerdict = iota // nothing to act on
	signonOn                           // XUS AV CODE: DUZ > 0
	signonExpired                      // XUS AV CODE: DUZ 0 with the expired flag (third line "1")
	signonCVC                          // XUS CVC: first line exactly "0" (signs on only a session marked changing; the caller decides)
)

// judgeSignon judges a reply to the request named rpc. clean and mErr come
// from the scanner; prefix is DataPrefix(); complete reports the reply has
// ended. decided reports the verdict is final (enough bytes seen, or the
// reply is complete); before that, the caller keeps feeding.
func judgeSignon(rpc string, clean, mErr bool, prefix []byte, complete bool) (v signonVerdict, decided bool) {
	var judge func([]byte) (signonVerdict, bool)
	switch rpc {
	case "XUS AV CODE":
		judge = xwbsJudgeAV
	case "XUS CVC":
		judge = xwbsJudgeCVC
	default:
		return signonNone, true
	}
	if !clean || mErr {
		return signonNone, true
	}
	if len(prefix) > xwbsPrefixMax {
		prefix = prefix[:xwbsPrefixMax]
	}
	v, decided = judge(prefix)
	if !decided && (complete || len(prefix) >= xwbsPrefixMax) {
		return signonNone, true
	}
	return v, decided
}

// xwbsJudgeAV judges an XUS AV CODE reply's data: a first line matching
// ^[1-9][0-9]{0,14}\r\n signs on; a first line 0\r\n and a third line
// 1\r\n is an expired verify code. Undecided means more bytes could decide.
func xwbsJudgeAV(p []byte) (signonVerdict, bool) {
	if len(p) == 0 {
		return signonNone, false
	}
	if p[0] == '0' {
		head, ok := xwbsExact(p, "0\r\n")
		if !ok {
			return signonNone, head
		}
		rest := p[3:]
		i := bytes.Index(rest, []byte("\r\n"))
		if i < 0 {
			return signonNone, false
		}
		if v, ok := xwbsExact(rest[i+2:], "1\r\n"); !ok {
			return signonNone, v
		}
		return signonExpired, true
	}
	if p[0] < '1' || p[0] > '9' {
		return signonNone, true
	}
	for i := 1; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= '0' && c <= '9' && i < 15:
		case c == '\r':
			if i+1 == len(p) {
				return signonNone, false
			}
			if p[i+1] == '\n' {
				return signonOn, true
			}
			return signonNone, true
		default:
			return signonNone, true
		}
	}
	return signonNone, false
}

// xwbsJudgeCVC judges an XUS CVC reply's data: a first line exactly 0\r\n.
func xwbsJudgeCVC(p []byte) (signonVerdict, bool) {
	if decided, ok := xwbsExact(p, "0\r\n"); !ok {
		return signonNone, decided
	}
	return signonCVC, true
}

// xwbsExact reports whether p begins with want. When it does not, decided
// reports that no further bytes could make it.
func xwbsExact(p []byte, want string) (decided, ok bool) {
	if len(p) >= len(want) {
		ok = string(p[:len(want)]) == want
		return true, ok
	}
	return string(p) != want[:len(p)], false
}

// vistaProxyLine is the PROXY protocol v1 line for a session: TCP4 when
// both addresses are IPv4 (an IPv4-mapped address counts as IPv4), else
// TCP6 with an IPv4 address written IPv4-mapped; zones removed. Ends
// "\r\n". An invalid address yields the UNKNOWN line.
func vistaProxyLine(client, server netip.AddrPort) string {
	c := client.Addr().WithZone("").Unmap()
	s := server.Addr().WithZone("").Unmap()
	if !c.IsValid() || !s.IsValid() {
		return "PROXY UNKNOWN\r\n"
	}
	family := "TCP4"
	if !c.Is4() || !s.Is4() {
		family = "TCP6"
		c = netip.AddrFrom16(c.As16())
		s = netip.AddrFrom16(s.As16())
	}
	return "PROXY " + family + " " + c.String() + " " + s.String() + " " +
		strconv.Itoa(int(client.Port())) + " " + strconv.Itoa(int(server.Port())) + "\r\n"
}

// vistaClientKey is the per-client cap key: the address's /32 (IPv4, after
// unmapping an IPv4-mapped IPv6 address) or /64 (IPv6), zone removed. An
// invalid address yields the zero Prefix.
func vistaClientKey(a netip.Addr) netip.Prefix {
	a = a.WithZone("").Unmap()
	bits := 64
	if a.Is4() {
		bits = 32
	}
	p, _ := a.Prefix(bits)
	return p
}
