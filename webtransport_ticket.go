package janus

// Tickets carry the auth gate's decision to the relay. A WebTransport
// CONNECT carries no cookie, so on a gated site the descriptor, served
// behind the gate, mints a ticket for the signed-in user and the browser
// presents it in the URL it dials. HMAC-SHA256 under a per-process key
// (descriptor and relay are one process), 60 s, single use.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"sync"
	"time"
)

const (
	wtTicketTTL      = 60 * time.Second
	wtTicketNonceLen = 16
	// wtTicketMaxLive bounds the outstanding single-use records PER ROUTE, so
	// a flood on one gated route cannot starve minting on another (each route
	// is a separate tenant surface). Records self-expire after the TTL.
	wtTicketMaxLive = 4096
	wtTicketQuery   = "t"
)

// wtTicketSet mints and redeems tickets and remembers the redeemed nonces
// until they expire, so a ticket is good exactly once. The redeemed set is
// scoped per route.
type wtTicketSet struct {
	key     [32]byte
	mu      sync.Mutex
	used    map[string]map[[wtTicketNonceLen]byte]int64 // route → nonce → expiry (unix s)
	maxLive int                                         // per-route cap on outstanding records
	now     func() time.Time
}

func newWtTicketSet() *wtTicketSet {
	return &wtTicketSet{key: newTicketKey(), used: map[string]map[[wtTicketNonceLen]byte]int64{}, maxLive: wtTicketMaxLive, now: time.Now}
}

// randNonce draws a fresh ticket nonce; ok is false on a crypto/rand
// failure, so mint can fail soft rather than panic in the descriptor path.
func randNonce() (nonce [wtTicketNonceLen]byte, ok bool) {
	if _, err := rand.Read(nonce[:]); err != nil {
		return nonce, false
	}
	return nonce, true
}

// mint returns a ticket for user on the route, or "" when the route's
// outstanding set is full of unexpired records or crypto/rand fails.
func (s *wtTicketSet) mint(route, user string) string {
	now := s.now()
	s.mu.Lock()
	s.purgeLocked(now)
	if len(s.used[route]) >= s.maxLive {
		s.mu.Unlock()
		return ""
	}
	s.mu.Unlock()
	nonce, ok := randNonce()
	if !ok {
		return ""
	}
	exp := now.Add(wtTicketTTL).Unix()
	body := make([]byte, 0, wtTicketNonceLen+8+1+len(user))
	body = append(body, nonce[:]...)
	body = binary.BigEndian.AppendUint64(body, uint64(exp))
	body = append(body, byte(len(user)))
	body = append(body, user...)
	mac := s.sign(route, body)
	return base64.RawURLEncoding.EncodeToString(append(body, mac...))
}

// redeem checks a ticket for the route and marks its nonce used. It
// returns the user it was minted for.
func (s *wtTicketSet) redeem(ticket, route string) (user string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(ticket)
	if err != nil || len(raw) < wtTicketNonceLen+8+1+sha256.Size {
		return "", false
	}
	body, mac := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	if !hmac.Equal(mac, s.sign(route, body)) {
		return "", false
	}
	n := int(body[wtTicketNonceLen+8])
	if len(body) != wtTicketNonceLen+8+1+n {
		return "", false
	}
	exp := int64(binary.BigEndian.Uint64(body[wtTicketNonceLen : wtTicketNonceLen+8]))
	now := s.now()
	if now.Unix() >= exp {
		return "", false
	}
	var nonce [wtTicketNonceLen]byte
	copy(nonce[:], body[:wtTicketNonceLen])
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked(now)
	if _, seen := s.used[route][nonce]; seen {
		return "", false
	}
	if s.used[route] == nil {
		s.used[route] = map[[wtTicketNonceLen]byte]int64{}
	}
	s.used[route][nonce] = exp
	return string(body[wtTicketNonceLen+8+1:]), true
}

func (s *wtTicketSet) sign(route string, body []byte) []byte {
	h := hmac.New(sha256.New, s.key[:])
	h.Write([]byte(route))
	h.Write([]byte{0})
	h.Write(body)
	return h.Sum(nil)
}

func (s *wtTicketSet) purgeLocked(now time.Time) {
	cut := now.Unix()
	for route, nonces := range s.used {
		for nonce, exp := range nonces {
			if exp <= cut {
				delete(nonces, nonce)
			}
		}
		if len(nonces) == 0 {
			delete(s.used, route)
		}
	}
}
