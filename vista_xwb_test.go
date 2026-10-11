package janus

import (
	"bytes"
	"math/rand"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// xwbwSP is a short string; xwbwLP a long string of width 3.
func xwbwSP(s string) string { return string(rune(len(s))) + s }
func xwbwLP(s string) string { return xwbwPad(len(s), 3) + s }

func xwbwPad(n, width int) string {
	s := strconv.Itoa(n)
	return strings.Repeat("0", width-len(s)) + s
}

// xwbwValid holds RPC-HTTPS §3's four examples and the grammar's other forms.
var xwbwValid = []struct {
	name string
	msg  string
	want xwbMessage
}{
	{"TCPConnect", "[XWB]10304" + xwbwSP("TCPConnect") + "5" + "0" + xwbwLP("127.0.0.1") + "f" + "0" + xwbwLP("0") + "f" + "0" + xwbwLP("localhost") + "f" + "\x04", xwbMessage{Command: "TCPConnect"}},
	{"XUS INTRO MSG", "[XWB]11302" + xwbwSP("0") + xwbwSP("XUS INTRO MSG") + "5\x04", xwbMessage{RPC: "XUS INTRO MSG"}},
	{"XUS AV CODE", "[XWB]11302" + xwbwSP("0") + xwbwSP("XUS AV CODE") + "5" + "0" + xwbwLP(" ACCESS;VERIFY ") + "f" + "\x04", xwbMessage{RPC: "XUS AV CODE"}},
	{"#BYE#", "[XWB]10304\x05#BYE#\x04", xwbMessage{Command: "#BYE#"}},
	{"a value holding the end byte", "[XWB]11302" + xwbwSP("0") + xwbwSP("ORWU X") + "5" + "0" + xwbwLP("a\x04b") + "f" + "\x04", xwbMessage{RPC: "ORWU X"}},
	{"a list continued with t, then a literal", "[XWB]11302" + xwbwSP("0") + xwbwSP("ORWDX SAVE") + "5" + "2" + xwbwLP(`"ORCHECK",1`) + xwbwLP("x") + "t" + xwbwLP(`"ORCHECK",2`) + xwbwLP("y") + "f" + "0" + xwbwLP("5") + "f" + "\x04", xwbMessage{RPC: "ORWDX SAVE"}},
	{"a global array", "[XWB]11302" + xwbwSP("0") + xwbwSP("ORWU X") + "5" + "3" + xwbwLP("1") + xwbwLP("\x01") + "f" + "\x04", xwbMessage{RPC: "ORWU X"}},
	{"a list with an empty subscript", "[XWB]11302" + xwbwSP("0") + xwbwSP("ORWU X") + "5" + "2" + xwbwLP("") + "f" + "\x04", xwbMessage{RPC: "ORWU X"}},
	{"a reference and empty parameters", "[XWB]11302" + xwbwSP("0") + xwbwSP("ORWU X") + "5" + "1" + xwbwLP("DUZ") + "f" + "4f" + "5f" + "0" + xwbwLP("") + "f" + "\x04", xwbMessage{RPC: "ORWU X"}},
	{"width 9", "[XWB]11902" + xwbwSP("0") + xwbwSP("TIU X") + "5" + "0" + "000000003abc" + "f" + "\x04", xwbMessage{RPC: "TIU X"}},
	{"width 0 means 3", "[XWB]11002" + xwbwSP("0") + xwbwSP("TIU X") + "5" + "0" + "003abc" + "f" + "\x04", xwbMessage{RPC: "TIU X"}},
	{"width 1", "[XWB]11102" + xwbwSP("0") + xwbwSP("TIU X") + "5" + "0" + "3abc" + "f" + "\x04", xwbMessage{RPC: "TIU X"}},
	{"chunks 1 and 3, no chunk 5", "[XWB]11301" + xwbwSP("1.108") + xwbwSP("") + "2" + xwbwSP("0") + xwbwSP("XWB IM HERE") + "3" + "\x04", xwbMessage{RPC: "XWB IM HERE"}},
	{"a command with chunk 1", "[XWB]10301" + xwbwSP("") + xwbwSP("") + "4" + xwbwSP("#BYE#") + "\x04", xwbMessage{Command: "#BYE#"}},
}

func TestWalkXWBAcceptsEachForm(t *testing.T) {
	for _, c := range xwbwValid {
		t.Run(c.name, func(t *testing.T) {
			got, err := walkXWB([]byte(c.msg))
			if err != nil {
				t.Fatalf("walkXWB: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestWalkXWBRefuses(t *testing.T) {
	rpc := "[XWB]11302" + xwbwSP("0") + xwbwSP("X")
	cases := map[string]string{
		"empty":                            "",
		"a bare #BYE#":                     "#BYE#",
		"a bare #BYE# with its end byte":   "#BYE#\x04",
		"the old callback broker":          "{XWB}1130",
		"M2M":                              "<?xml version",
		"a header cut short":               "[XWB]113",
		"version 2":                        "[XWB]21302" + xwbwSP("0") + xwbwSP("X") + "5\x04",
		"version 0":                        "[XWB]01302" + xwbwSP("0") + xwbwSP("X") + "5\x04",
		"type 2":                           "[XWB]12302" + xwbwSP("0") + xwbwSP("X") + "5\x04",
		"a width that is no digit":         "[XWB]11a02" + xwbwSP("0") + xwbwSP("X") + "5\x04",
		"a header asking no reply":         "[XWB]11312" + xwbwSP("0") + xwbwSP("X") + "5\x04",
		"a chunk out of order":             "[XWB]11303" + "2" + xwbwSP("0") + xwbwSP("X") + "5\x04",
		"chunk 1 after chunk 2":            rpc + "1" + xwbwSP("") + xwbwSP("") + "5\x04",
		"chunk 2 repeated":                 rpc + "2" + xwbwSP("0") + xwbwSP("Y") + "5\x04",
		"chunk 3 repeated":                 rpc + "33\x04",
		"an RPC without chunk 2":           "[XWB]11305\x04",
		"an RPC with only chunk 3":         "[XWB]11303\x04",
		"an RPC with chunk 4":              rpc + "4" + xwbwSP("#BYE#") + "\x04",
		"an RPC with an empty name":        "[XWB]11302" + xwbwSP("0") + xwbwSP("") + "5\x04",
		"a command without chunk 4":        "[XWB]10305\x04",
		"a command with chunk 2":           "[XWB]10302" + xwbwSP("0") + xwbwSP("X") + "4" + xwbwSP("#BYE#") + "\x04",
		"another command":                  "[XWB]10304" + xwbwSP("XWB IM HERE") + "\x04",
		"a command in another case":        "[XWB]10304" + xwbwSP("tcpconnect") + "\x04",
		"a command with a trailing space":  "[XWB]10304" + xwbwSP("#BYE# ") + "\x04",
		"chunk 4 followed by chunk 3":      "[XWB]10304" + xwbwSP("#BYE#") + "3\x04",
		"chunk 6":                          rpc + "6\x04",
		"chunk 0":                          rpc + "0\x04",
		"another end byte":                 rpc + "\x05",
		"a NUL end byte":                   rpc + "\x00",
		"no end byte":                      rpc,
		"no end byte after the parameters": rpc + "5" + "0" + xwbwLP("x") + "f",
		"trailing bytes":                   rpc + "5\x04" + "x",
		"two end bytes":                    rpc + "5\x04\x04",
		"a second message":                 rpc + "5\x04" + "[XWB]10304\x05#BYE#\x04",
		"a parameter of no type":           rpc + "5" + "9" + "f\x04",
		"a parameter of type 6":            rpc + "5" + "6" + "f\x04",
		"a literal continued with t":       rpc + "5" + "0" + xwbwLP("x") + "t" + xwbwLP("y") + "f\x04",
		"a literal not closed":             rpc + "5" + "0" + xwbwLP("x") + "x\x04",
		"a literal closed by the end byte": rpc + "5" + "0" + xwbwLP("x") + "\x04",
		"a list entry followed by x":       rpc + "5" + "2" + xwbwLP("1") + xwbwLP("a") + "x\x04",
		"a length that is no number":       rpc + "5" + "0" + "0x3abc" + "f\x04",
		"a long string past the body":      rpc + "5" + "0" + "010abc" + "f\x04",
		"a short string past the body":     "[XWB]11302" + xwbwSP("0") + "\x20XUS" + "5\x04",
		"a huge declared length":           "[XWB]11902" + xwbwSP("0") + xwbwSP("X") + "5" + "0" + "999999999" + "abcdef\x04",
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if m, err := walkXWB([]byte(s)); err == nil {
				t.Fatalf("walkXWB accepted %q as %+v", s, m)
			}
		})
	}
}

func TestWalkXWBRefusesEveryTruncation(t *testing.T) {
	for _, c := range xwbwValid {
		for i := range len(c.msg) {
			if m, err := walkXWB([]byte(c.msg[:i])); err == nil {
				t.Fatalf("%s: accepted the first %d bytes as %+v", c.name, i, m)
			}
		}
	}
}

func TestWalkXWBNeverAllocatesFromADeclaredLength(t *testing.T) {
	body := []byte("[XWB]11902" + xwbwSP("0") + xwbwSP("X") + "5" + "0" + "999999999" + "abcdef\x04")
	if len(body) > 32 {
		t.Fatalf("the body is %d bytes", len(body))
	}
	allocs := testing.AllocsPerRun(100, func() {
		if _, err := walkXWB(body); err == nil {
			t.Fatal("walkXWB accepted a 999,999,999-byte declaration")
		}
	})
	if allocs > 8 { // the error's own formatting (more under -race), nothing more
		t.Fatalf("walkXWB allocated %v times", allocs)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range 100 {
		_, _ = walkXWB(body)
	}
	runtime.ReadMemStats(&after)
	if per := (after.TotalAlloc - before.TotalAlloc) / 100; per > 1024 {
		t.Fatalf("walkXWB allocated %d bytes per run", per)
	}
	ok := []byte(xwbwValid[0].msg)
	if allocs := testing.AllocsPerRun(100, func() { _, _ = walkXWB(ok) }); allocs != 0 {
		t.Fatalf("walking a valid command allocated %v times", allocs)
	}
}

func FuzzWalkXWB(f *testing.F) {
	for _, c := range xwbwValid {
		f.Add([]byte(c.msg))
	}
	f.Add([]byte("#BYE#"))
	f.Add([]byte("[XWB]11902" + xwbwSP("0") + xwbwSP("X") + "5" + "0" + "999999999" + "abcdef\x04"))
	f.Fuzz(func(t *testing.T, body []byte) {
		m, err := walkXWB(body)
		if err != nil {
			return
		}
		if !bytes.HasPrefix(body, []byte("[XWB]")) || body[len(body)-1] != xwbEnd {
			t.Fatalf("accepted %q", body)
		}
		if (m.Command == "") == (m.RPC == "") {
			t.Fatalf("accepted %q as %+v", body, m)
		}
		for i := range len(body) {
			if _, err := walkXWB(body[:i]); err == nil {
				t.Fatalf("accepted %q and its first %d bytes", body, i)
			}
		}
	})
}

// xwbsReplies are RPC-HTTPS §3's measured replies.
var xwbsReplies = []struct {
	name  string
	reply string
	clean bool
	mErr  bool
	data  string
}{
	{"TCPConnect", "\x00\x00accept\x04", true, false, "accept"},
	{"XWB IM HERE", "\x00\x001\x04", true, false, "1"},
	{"XWB GET BROKER INFO", "\x00\x00180\r\n\x04", true, false, "180\r\n"},
	{"XUS SIGNON SETUP", "\x00\x00pop\r\nENV\r\nENV\r\n/dev/null\r\n5\r\n0\r\nFOIA.DOMAIN.EXT\r\n0\r\n\x04", true, false, "pop\r\nENV\r\nENV\r\n/dev/null\r\n5\r\n0\r\nFOIA.DOMAIN.EXT\r\n0\r\n"},
	{"XUS AV CODE", "\x00\x00100\r\n0\r\n0\r\n\r\n0\r\n0\r\n\r\nGood evening MANAGER,SITE\r\n\x04", true, false, "100\r\n0\r\n0\r\n\r\n0\r\n0\r\n\r\nGood evening MANAGER,SITE\r\n"},
	{"ORWU USERINFO", "\x00\x00100^MANAGER,SITE^0^0^0^0^0^300^20^1^1^5^FOIA.DOMAIN.EXT^0^180^1^0^0^0^0\x04", true, false, "100^MANAGER,SITE^0^0^0^0^0^300^20^1^1^5^FOIA.DOMAIN.EXT^0^180^1^0^0^0^0"},
	{"refused", "\x3ARemote Procedure 'ORWU USERINFO' cannot run before sign-on\x00\x04", false, false, ""},
	{"an RPC error", "\x00\x05oops!\x04", false, false, ""},
	{"an M error", "\x00\x00\x18M  ERROR=CAPI+16^XWBPRS,4 actual parameters for IMHERE^XWBLIB,M58,-M58\r\nLAST REF=^XWB(8994)\x04\x04", true, true, "\x18M  ERROR=CAPI+16^XWBPRS,4 actual parameters for IMHERE^XWBLIB,M58,-M58\r\nLAST REF=^XWB(8994)"},
	{"#BYE#", "\x00\x00#BYE#\x04", true, false, "#BYE#"},
	{"empty data", "\x00\x00\x04", true, false, ""},
}

// xwbsScan feeds pieces to a fresh scanner until it is done and returns it
// with how many bytes of the pieces it consumed.
func xwbsScan(t *testing.T, pieces [][]byte) (*xwbReplyScanner, int, bool) {
	t.Helper()
	s := &xwbReplyScanner{}
	used := 0
	for _, p := range pieces {
		n, done := s.Feed(p)
		used += n
		if n < len(p) && !done {
			t.Fatalf("Feed consumed %d of %d bytes without ending", n, len(p))
		}
		if done {
			return s, used, true
		}
	}
	return s, used, false
}

func xwbsSplits(b []byte, rng *rand.Rand) [][]byte {
	var pieces [][]byte
	for len(b) > 0 {
		k := 1 + rng.Intn(len(b))
		pieces = append(pieces, b[:k])
		b = b[k:]
	}
	return pieces
}

func TestXWBReplyScannerFindsEachReplysEnd(t *testing.T) {
	if n := len("Remote Procedure 'ORWU USERINFO' cannot run before sign-on"); n != 0x3A {
		t.Fatalf("the security message is %d bytes", n)
	}
	rng := rand.New(rand.NewSource(11))
	for _, c := range xwbsReplies {
		t.Run(c.name, func(t *testing.T) {
			stream := []byte(c.reply + "[left]")
			whole := [][]byte{stream}
			var bytewise [][]byte
			for i := range stream {
				bytewise = append(bytewise, stream[i:i+1])
			}
			feeds := map[string][][]byte{"whole": whole, "byte by byte": bytewise}
			for i := range 20 {
				feeds["random split "+strconv.Itoa(i)] = xwbsSplits(stream, rng)
			}
			for how, pieces := range feeds {
				s, used, done := xwbsScan(t, pieces)
				if !done || used != len(c.reply) || s.Total() != int64(len(c.reply)) {
					t.Fatalf("%s: done %v, used %d, total %d; want %d", how, done, used, s.Total(), len(c.reply))
				}
				if !s.PastShorts() || s.Clean() != c.clean || s.MError() != c.mErr {
					t.Fatalf("%s: past %v clean %v mErr %v", how, s.PastShorts(), s.Clean(), s.MError())
				}
				want := c.data[:min(len(c.data), 64)]
				if string(s.DataPrefix()) != want {
					t.Fatalf("%s: prefix %q, want %q", how, s.DataPrefix(), want)
				}
				if n, done := s.Feed([]byte("more")); n != 0 || !done {
					t.Fatalf("%s: Feed after the end consumed %d, done %v", how, n, done)
				}
			}
		})
	}
}

func TestXWBReplyScannerWaits(t *testing.T) {
	cases := map[string]struct {
		parts      []string
		pastShorts bool
		mErr       bool
	}{
		"nothing":                       {nil, false, false},
		"inside the security message":   {[]string{"\x05ab"}, false, false},
		"between the short strings":     {[]string{"\x00"}, false, false},
		"past the short strings":        {[]string{"\x00\x00"}, true, false},
		"inside the data":               {[]string{"\x00\x00", "abc"}, true, false},
		"a data byte holding \\r":       {[]string{"\x00\x00abc\r"}, true, false},
		"an M error before its 2nd end": {[]string{"\x00\x00\x18M  ERROR=X\x04"}, true, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := &xwbReplyScanner{}
			total := 0
			for _, p := range c.parts {
				n, done := s.Feed([]byte(p))
				if done || n != len(p) {
					t.Fatalf("Feed(%q) = %d, %v", p, n, done)
				}
				total += n
			}
			if s.Total() != int64(total) || s.PastShorts() != c.pastShorts || s.MError() != c.mErr {
				t.Fatalf("total %d past %v mErr %v", s.Total(), s.PastShorts(), s.MError())
			}
		})
	}
}

func TestXWBReplyScannerEndsAnMErrorBeforeAStrayByte(t *testing.T) {
	s := &xwbReplyScanner{}
	if n, done := s.Feed([]byte("\x00\x00\x18M\x04")); n != 5 || done {
		t.Fatalf("Feed = %d, %v", n, done)
	}
	if n, done := s.Feed([]byte("X")); n != 0 || !done {
		t.Fatalf("Feed = %d, %v; the stray byte must be leftover", n, done)
	}
	if s.Total() != 5 {
		t.Fatalf("total %d", s.Total())
	}
}

func TestXWBReplyScannerKeepsOnlyTheFirst64Bytes(t *testing.T) {
	data := strings.Repeat("0123456789", 10)
	s := &xwbReplyScanner{}
	in := []byte("\x00\x00" + data[:30])
	s.Feed(in)
	in[2] = 'X' // the prefix is the scanner's own copy
	s.Feed([]byte(data[30:] + "\x04"))
	if string(s.DataPrefix()) != data[:64] {
		t.Fatalf("prefix %q", s.DataPrefix())
	}
}

func TestJudgeSignon(t *testing.T) {
	cases := []struct {
		name  string
		rpc   string
		reply string // the whole reply, short strings included
		want  signonVerdict
	}{
		{"a DUZ", "XUS AV CODE", "\x00\x00100\r\n0\r\n0\r\n\r\n0\r\n0\r\n\r\nGood evening MANAGER,SITE\r\n\x04", signonOn},
		{"a one-digit DUZ", "XUS AV CODE", "\x00\x005\r\n\x04", signonOn},
		{"a 15-digit DUZ", "XUS AV CODE", "\x00\x00123456789012345\r\n\x04", signonOn},
		{"a 16-digit DUZ", "XUS AV CODE", "\x00\x001234567890123456\r\n\x04", signonNone},
		{"a long first line", "XUS AV CODE", "\x00\x00" + strings.Repeat("9", 80) + "\r\n\x04", signonNone},
		{"DUZ 0", "XUS AV CODE", "\x00\x000\r\n0\r\n0\r\n\r\n\x04", signonNone},
		{"an expired verify code", "XUS AV CODE", "\x00\x000\r\n0\r\n1\r\n\r\n\x04", signonExpired},
		{"expired, with a text second line", "XUS AV CODE", "\x00\x000\r\nsome text\r\n1\r\n\x04", signonExpired},
		{"expired, with an empty second line", "XUS AV CODE", "\x00\x000\r\n\r\n1\r\n\x04", signonExpired},
		{"a third line 11", "XUS AV CODE", "\x00\x000\r\n0\r\n11\r\n\x04", signonNone},
		{"a third line 1 unended", "XUS AV CODE", "\x00\x000\r\n0\r\n1\x04", signonNone},
		{"no third line", "XUS AV CODE", "\x00\x000\r\n0\r\n\x04", signonNone},
		{"a second line past 64 bytes", "XUS AV CODE", "\x00\x000\r\n" + strings.Repeat("x", 70) + "\r\n1\r\n\x04", signonNone},
		{"00", "XUS AV CODE", "\x00\x0000\r\n\x04", signonNone},
		{"01", "XUS AV CODE", "\x00\x0001\r\n\x04", signonNone},
		{"1E3", "XUS AV CODE", "\x00\x001E3\r\n\x04", signonNone},
		{"-1", "XUS AV CODE", "\x00\x00-1\r\n\x04", signonNone},
		{"+1", "XUS AV CODE", "\x00\x00+1\r\n\x04", signonNone},
		{" 1", "XUS AV CODE", "\x00\x00 1\r\n\x04", signonNone},
		{"1.0", "XUS AV CODE", "\x00\x001.0\r\n\x04", signonNone},
		{"1 and a bare newline", "XUS AV CODE", "\x00\x001\n\x04", signonNone},
		{"1 and a bare return", "XUS AV CODE", "\x00\x001\rX\r\n\x04", signonNone},
		{"1 unended", "XUS AV CODE", "\x00\x001\x04", signonNone},
		{"1 and a return at the end", "XUS AV CODE", "\x00\x001\r\x04", signonNone},
		{"empty data", "XUS AV CODE", "\x00\x00\x04", signonNone},
		{"a security message", "XUS AV CODE", "\x01X\x00100\r\n\x04", signonNone},
		{"an RPC error", "XUS AV CODE", "\x00\x01X100\r\n\x04", signonNone},
		{"an M error", "XUS AV CODE", "\x00\x00\x18M  ERROR=1\r\n\x04\x04", signonNone},
		{"a CVC change", "XUS CVC", "\x00\x000\r\n\x04", signonCVC},
		{"a CVC change with text", "XUS CVC", "\x00\x000\r\nchanged\r\n\x04", signonCVC},
		{"a CVC refusal", "XUS CVC", "\x00\x001\r\nVerify code too short\r\n\x04", signonNone},
		{"a CVC 00", "XUS CVC", "\x00\x0000\r\n\x04", signonNone},
		{"a CVC 0 unended", "XUS CVC", "\x00\x000\x04", signonNone},
		{"a CVC 0 and a bare newline", "XUS CVC", "\x00\x000\n\x04", signonNone},
		{"a CVC M error", "XUS CVC", "\x00\x00\x18M\x04\x04", signonNone},
		{"a CVC security message", "XUS CVC", "\x01X\x000\r\n\x04", signonNone},
		{"another RPC", "XUS SIGNON SETUP", "\x00\x00100\r\n\x04", signonNone},
		{"a lower-case name", "xus av code", "\x00\x00100\r\n\x04", signonNone},
		{"a trailing space", "XUS AV CODE ", "\x00\x00100\r\n\x04", signonNone},
		{"a leading space", " XUS AV CODE", "\x00\x00100\r\n\x04", signonNone},
		{"a doubled space", "XUS AV  CODE", "\x00\x00100\r\n\x04", signonNone},
		{"a lower-case CVC", "xus cvc", "\x00\x000\r\n\x04", signonNone},
		{"a CVC with a trailing space", "XUS CVC ", "\x00\x000\r\n\x04", signonNone},
		{"no name", "", "\x00\x00100\r\n\x04", signonNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Fed a byte at a time, the first decision is final and right.
			s := &xwbReplyScanner{}
			var first signonVerdict
			decidedAt := -1
			for i := range len(c.reply) {
				_, done := s.Feed([]byte{c.reply[i]})
				v, decided := judgeSignon(c.rpc, s.Clean(), s.MError(), s.DataPrefix(), done)
				if decided && decidedAt < 0 {
					first, decidedAt = v, i
				}
				if decidedAt >= 0 && v != first {
					t.Fatalf("at byte %d the verdict changed from %v to %v", i, first, v)
				}
				if done && !decided {
					t.Fatalf("undecided on a complete reply")
				}
			}
			if decidedAt < 0 || first != c.want {
				t.Fatalf("got %v (decided at %d), want %v", first, decidedAt, c.want)
			}
			// Fed whole, the same.
			s = &xwbReplyScanner{}
			_, done := s.Feed([]byte(c.reply))
			if v, decided := judgeSignon(c.rpc, s.Clean(), s.MError(), s.DataPrefix(), done); !decided || v != c.want {
				t.Fatalf("whole: got %v, %v", v, decided)
			}
		})
	}
}

func TestJudgeSignonDecidesEarly(t *testing.T) {
	cases := []struct {
		name    string
		rpc     string
		prefix  string
		want    signonVerdict
		decided bool
	}{
		{"a DUZ line decides at once", "XUS AV CODE", "5\r\n", signonOn, true},
		{"a DUZ waits for its line's end", "XUS AV CODE", "5\r", signonNone, false},
		{"a 0 line waits for the third line", "XUS AV CODE", "0\r\n", signonNone, false},
		{"a 0 line waits through the third line", "XUS AV CODE", "0\r\n0\r\n1\r", signonNone, false},
		{"an expired flag decides", "XUS AV CODE", "0\r\n0\r\n1\r\n", signonExpired, true},
		{"a third line 0 decides", "XUS AV CODE", "0\r\n0\r\n0", signonNone, true},
		{"00 decides", "XUS AV CODE", "00", signonNone, true},
		{"a letter decides", "XUS AV CODE", "1E", signonNone, true},
		{"64 bytes without a line decide", "XUS AV CODE", "0\r\n" + strings.Repeat("x", 61), signonNone, true},
		{"nothing yet", "XUS AV CODE", "", signonNone, false},
		{"a CVC 0 line decides", "XUS CVC", "0\r\n", signonCVC, true},
		{"a CVC 0 waits", "XUS CVC", "0", signonNone, false},
		{"a CVC 1 decides", "XUS CVC", "1", signonNone, true},
		{"another RPC decides", "ORWU USERINFO", "", signonNone, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, decided := judgeSignon(c.rpc, true, false, []byte(c.prefix), false)
			if v != c.want || decided != c.decided {
				t.Fatalf("got %v, %v; want %v, %v", v, decided, c.want, c.decided)
			}
		})
	}
	if v, decided := judgeSignon("XUS AV CODE", false, false, nil, false); v != signonNone || !decided {
		t.Fatalf("an unclean reply: %v, %v", v, decided)
	}
	if v, decided := judgeSignon("XUS AV CODE", true, true, []byte("\x18"), false); v != signonNone || !decided {
		t.Fatalf("an M error: %v, %v", v, decided)
	}
}

func TestVistaProxyLine(t *testing.T) {
	cases := []struct {
		name, client, server, want string
	}{
		{"TCP4", "1.2.3.4:51000", "5.6.7.8:443", "PROXY TCP4 1.2.3.4 5.6.7.8 51000 443\r\n"},
		{"TCP6", "[2001:db8::1]:51000", "[2001:db8::2]:443", "PROXY TCP6 2001:db8::1 2001:db8::2 51000 443\r\n"},
		{"an IPv4 client, an IPv6 server", "1.2.3.4:51000", "[2001:db8::2]:443", "PROXY TCP6 ::ffff:1.2.3.4 2001:db8::2 51000 443\r\n"},
		{"an IPv6 client, an IPv4 server", "[2001:db8::1]:51000", "5.6.7.8:443", "PROXY TCP6 2001:db8::1 ::ffff:5.6.7.8 51000 443\r\n"},
		{"zones", "[fe80::1%en0]:51000", "[fe80::2%en0]:443", "PROXY TCP6 fe80::1 fe80::2 51000 443\r\n"},
		{"an IPv4-mapped client, an IPv4 server", "[::ffff:1.2.3.4]:51000", "5.6.7.8:443", "PROXY TCP4 1.2.3.4 5.6.7.8 51000 443\r\n"},
		{"an IPv4-mapped client, an IPv6 server", "[::ffff:1.2.3.4]:51000", "[2001:db8::2]:443", "PROXY TCP6 ::ffff:1.2.3.4 2001:db8::2 51000 443\r\n"},
		{"a trusted proxy's client, port 0", "9.9.9.9:0", "5.6.7.8:443", "PROXY TCP4 9.9.9.9 5.6.7.8 0 443\r\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := vistaProxyLine(netip.MustParseAddrPort(c.client), netip.MustParseAddrPort(c.server))
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
	if got := vistaProxyLine(netip.AddrPort{}, netip.MustParseAddrPort("5.6.7.8:443")); got != "PROXY UNKNOWN\r\n" {
		t.Fatalf("an invalid client: %q", got)
	}
}

func TestVistaClientKey(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4":                  "1.2.3.4/32",
		"::ffff:1.2.3.4":           "1.2.3.4/32",
		"2001:db8:1:2:3:4:5:6":     "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::1%en0": "2001:db8:1:2::/64",
		"fe80::1%en0":              "fe80::/64",
	}
	for in, want := range cases {
		if got := vistaClientKey(netip.MustParseAddr(in)); got.String() != want {
			t.Errorf("vistaClientKey(%s) = %s, want %s", in, got, want)
		}
	}
	if got := vistaClientKey(netip.Addr{}); got.IsValid() {
		t.Errorf("vistaClientKey of the zero Addr = %s", got)
	}
}
