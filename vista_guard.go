package janus

// The message guard of a route to a stock RPC Broker (a tcp target;
// docs/20261114-142034-capability-vista-tcp.md). VA's parser builds a list
// parameter's subscripts as M code and evaluates a reference parameter
// before it checks the RPC or the user, and an M error partway through a
// message leaves its rest to be read as a new message. em closes this in
// VistA; for a stock broker Janus closes it before a message leaves Janus.
// The shape rules (1–4) judge every message at admission; the session
// rules (5–7) judge a request when it leaves its session's queue. A
// message that breaks one is answered with a broker security reply
// carrying a fixed message, and never sent.

import (
	"bytes"
	"net/netip"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Guard rules, as the guard_refused counters name them.
const (
	vistaGuardListSubscript = "list_subscript" // rule 1
	vistaGuardLiteralBound  = "literal_bound"  // rule 2
	vistaGuardFramingByte   = "framing_byte"   // rule 3
	vistaGuardCommandParams = "command_params" // rule 4
	vistaGuardSignonRPC     = "signon_rpc"     // rule 5
	vistaGuardReference     = "reference"      // rule 6
	vistaGuardSignonRerun   = "signon_rerun"   // rule 7
)

var vistaGuardRules = []string{vistaGuardCommandParams, vistaGuardListSubscript, vistaGuardLiteralBound,
	vistaGuardFramingByte, vistaGuardSignonRPC, vistaGuardReference, vistaGuardSignonRerun}

// The fixed security messages: no client byte is ever echoed, and the
// rule broken is in the guard_refused counters, not the reply.
const (
	vistaMsgInvalid      = "Invalid request"                           // rules 1–4
	vistaMsgUnauthorized = "Unauthorized request"                      // rules 5–7
	vistaMsgFailedSignon = "Too many failed sign-ons; try again later" // the failed sign-on hold
)

func vistaGuardMessage(rule string) string {
	switch rule {
	case vistaGuardSignonRPC, vistaGuardReference, vistaGuardSignonRerun:
		return vistaMsgUnauthorized
	}
	return vistaMsgInvalid
}

// vistaSecurityReply is a broker reply carrying msg as its security
// message, no error, and no data: LEN, msg, \0, \x04.
func vistaSecurityReply(msg string) []byte {
	b := make([]byte, 0, len(msg)+3)
	b = append(b, byte(len(msg)))
	b = append(b, msg...)
	return append(b, 0, xwbEnd)
}

// The literal bounds (rule 2), under YottaDB's, GT.M's and IRIS's key and
// string maxima, so that parsing a message cannot raise an M error.
const (
	xwbgMaxDigits    = 15    // digits in a number
	xwbgMaxString    = 200   // characters in a quoted string
	xwbgMaxSubscript = 240   // bytes in one entry's subscript
	xwbgMaxLevels    = 8     // literals in one entry's subscript
	xwbgMaxValue     = 32000 // bytes in a value
)

// vistaPreSignon are the RPCs a session may run before sign-on (rule 5).
var vistaPreSignon = map[string]bool{
	"XUS SIGNON SETUP": true, "XUS INTRO MSG": true, "XUS AV CODE": true, "XUS AV HELP": true,
	"XUS CVC": true, "XUS SET SHARED": true, "XWB GET BROKER INFO": true, "XWB IM HERE": true,
	"XWB IS RPC AVAILABLE": true,
}

// vistaSignonRPCs sign a session on; a signed-on session runs none (rule 7).
var vistaSignonRPCs = map[string]bool{
	"XUS SIGNON SETUP": true, "XUS AV CODE": true, "XUS CVC": true, "XUS ESSO VALIDATE": true,
}

// xwbgFraming reports a field holding a framing byte (rule 3): the end
// byte, or the bytes that begin a message.
func xwbgFraming(f []byte) bool {
	return bytes.IndexByte(f, xwbEnd) >= 0 || bytes.Contains(f, []byte("[XWB]"))
}

// vistaShapeGuard judges a message by the shape rules (1–4) and returns
// the rule it breaks, or "".
func vistaShapeGuard(m xwbMessage, d *xwbDetail) string {
	for _, s := range d.Shorts {
		if xwbgFraming(s) {
			return vistaGuardFramingByte
		}
	}
	for _, p := range d.Params {
		switch p.Type {
		case '0', '1':
			if xwbgFraming(p.Value) {
				return vistaGuardFramingByte
			}
			if len(p.Value) > xwbgMaxValue {
				return vistaGuardLiteralBound
			}
		case '2', '3':
			for _, e := range p.Entries {
				if xwbgFraming(e.Sub) || xwbgFraming(e.Value) {
					return vistaGuardFramingByte
				}
				if len(e.Sub) == 0 {
					continue // an empty subscript ends its entry with no value
				}
				if rule := xwbgSubscript(e.Sub); rule != "" {
					return rule
				}
				if len(e.Value) > xwbgMaxValue {
					return vistaGuardLiteralBound
				}
			}
		}
	}
	switch m.Command {
	case "TCPConnect":
		// Address, port, and name, joined by the broker with ^.
		if !d.HasParams || len(d.Params) != 3 {
			return vistaGuardCommandParams
		}
		for _, p := range d.Params {
			if p.Type != '0' || bytes.IndexByte(p.Value, '^') >= 0 {
				return vistaGuardCommandParams
			}
		}
		if !xwbgDigits(d.Params[1].Value) {
			return vistaGuardCommandParams
		}
	case "#BYE#":
		if d.HasParams {
			return vistaGuardCommandParams
		}
	}
	return ""
}

func xwbgDigits(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// xwbgSubscript judges one entry's subscript as LIT^ZEMXWB reads it:
// literals, numbers or quoted strings ("" a quote inside one), separated
// by commas (rule 1), each within its bound and the whole within its
// length and levels (rule 2). A shape fault outranks a bound.
func xwbgSubscript(x []byte) string {
	bound := len(x) > xwbgMaxSubscript
	levels := 0
	for i := 0; ; {
		j, size, ok := xwbgLiteral(x, i)
		if !ok {
			return vistaGuardListSubscript
		}
		levels++
		if size < 0 {
			bound = true
		}
		if j == len(x) {
			break
		}
		if x[j] != ',' {
			return vistaGuardListSubscript
		}
		i = j + 1
	}
	if bound || levels > xwbgMaxLevels {
		return vistaGuardLiteralBound
	}
	return ""
}

// xwbgLiteral reads the literal at x[i:]: a quoted string, or a number
// (an optional -, then digits with an optional fraction, or a fraction
// alone). It returns the index after it and ok; size is -1 when the
// literal passes its bound.
func xwbgLiteral(x []byte, i int) (j, size int, ok bool) {
	if i >= len(x) {
		return 0, 0, false
	}
	if x[i] == '"' {
		chars := 0
		for j = i + 1; ; {
			k := bytes.IndexByte(x[j:], '"')
			if k < 0 {
				return 0, 0, false
			}
			chars += k
			j += k + 1
			if j < len(x) && x[j] == '"' {
				chars++ // a doubled quote is one character
				j++
				continue
			}
			break
		}
		if chars > xwbgMaxString {
			return j, -1, true
		}
		return j, chars, true
	}
	j = i
	if x[j] == '-' {
		j++
	}
	whole := xwbgRun(x, j)
	j += whole
	frac := 0
	if j < len(x) && x[j] == '.' {
		frac = xwbgRun(x, j+1)
		if frac == 0 {
			return 0, 0, false
		}
		j += 1 + frac
	}
	if whole+frac == 0 {
		return 0, 0, false
	}
	if whole+frac > xwbgMaxDigits {
		return j, -1, true
	}
	return j, whole + frac, true
}

// xwbgRun is how many digits begin x[i:].
func xwbgRun(x []byte, i int) int {
	n := 0
	for i+n < len(x) && x[i+n] >= '0' && x[i+n] <= '9' {
		n++
	}
	return n
}

// xwbgVariable judges a reference parameter's value as REF^ZEMXWB does for
// a signed-on client: a local variable's name (% or a letter, then letters
// and digits), optionally with subscripts by rules 1–3 (rule 6).
func xwbgVariable(v []byte) bool {
	name := v
	if k := bytes.IndexByte(v, '('); k >= 0 {
		name = v[:k]
	}
	if len(name) == 0 || !(name[0] == '%' || xwbgAlpha(name[0])) {
		return false
	}
	for _, c := range name[1:] {
		if !xwbgAlpha(c) && !(c >= '0' && c <= '9') {
			return false
		}
	}
	if len(name) == len(v) {
		return true
	}
	if len(v) < len(name)+2 || v[len(v)-1] != ')' {
		return false
	}
	return xwbgSubscript(v[len(name)+1:len(v)-1]) == ""
}

func xwbgAlpha(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }

// vistaSessionGuard judges a request by the session rules (5–7) on the
// session's state when the request leaves the queue, and returns the rule
// it breaks, or "". Commands are not RPCs and pass.
func vistaSessionGuard(m xwbMessage, d *xwbDetail, signed bool) string {
	if m.RPC == "" {
		return ""
	}
	if !signed && !vistaPreSignon[m.RPC] {
		return vistaGuardSignonRPC
	}
	for _, p := range d.Params {
		if p.Type == '1' && (!signed || !xwbgVariable(p.Value)) {
			return vistaGuardReference
		}
	}
	if signed && vistaSignonRPCs[m.RPC] {
		return vistaGuardSignonRerun
	}
	return ""
}

// vistaFailures is one client key's failed sign-ons in the window, and
// the end of its hold once they reached the limit.
type vistaFailures struct {
	at        []time.Time
	holdUntil time.Time
}

// vistaFailedSignons counts failed sign-ons per client key on a tcp route:
// through one, every user is Janus's one address to Kernel, so Kernel's
// own lockout cannot tell them apart.
type vistaFailedSignons struct {
	mu   sync.Mutex
	keys map[netip.Prefix]*vistaFailures
}

// vistaFailedSignonsSweep is the map size past which a failure sweeps
// out every key whose window and hold have passed.
const vistaFailedSignonsSweep = 1024

// held reports that key's XUS AV CODE is refused now: its hold has not
// passed.
func (f *vistaFailedSignons) held(key netip.Prefix, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.keys[key]
	if e == nil || e.holdUntil.IsZero() {
		return false
	}
	if now.Before(e.holdUntil) {
		return true
	}
	delete(f.keys, key) // the hold has passed: the key starts afresh
	return false
}

// fail records a failed sign-on for key and reports that it began a hold.
func (f *vistaFailedSignons) fail(key netip.Prefix, max int, window time.Duration, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys == nil {
		f.keys = map[netip.Prefix]*vistaFailures{}
	}
	if len(f.keys) > vistaFailedSignonsSweep {
		for k, e := range f.keys {
			if !now.Before(e.holdUntil) && (len(e.at) == 0 || now.Sub(e.at[len(e.at)-1]) >= window) {
				delete(f.keys, k)
			}
		}
	}
	e := f.keys[key]
	if e == nil {
		e = &vistaFailures{}
		f.keys[key] = e
	}
	if now.Before(e.holdUntil) {
		return false // a request that left the queue before the hold began
	}
	kept := e.at[:0]
	for _, t := range e.at {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	e.at = append(kept, now)
	if len(e.at) < max {
		return false
	}
	e.at = nil
	e.holdUntil = now.Add(window)
	return true
}

// guardRefuse counts a guard refusal by rule and logs it at warn, at most
// one line per route and rule each 10 s; never a value.
func (rs *vistaRouteState) guardRefuse(rule string) {
	counterOf(&rs.c.guard, rule).Add(1)
	rs.warn("guard_"+rule, "janus vista: the message guard refused a request", zap.String("rule", rule))
}

// failedSignon counts a failed sign-on from key and starts its hold at
// the route's limit.
func (rs *vistaRouteState) failedSignon(key netip.Prefix, l *vistaLimits) {
	rs.c.failedSignons.Add(1)
	if rs.failures.fail(key, l.maxFailedSignons, l.failedSignonWindow, time.Now()) {
		rs.warn("failed_signon_hold", "janus vista: failed sign-ons reached the limit; the client is held",
			zap.String("client", key.String()), zap.Duration("hold", l.failedSignonWindow))
	}
}
