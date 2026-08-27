package janus

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	accessFactsKey                = "janus.access.facts"
	accessSubscribersRegistration = 4
	accessSubscribersProcess      = 64
	accessQueueEvents             = 128
	accessLineBytes               = 8192
	accessHeartbeat               = 15 * time.Second
	accessWriteDeadline           = 2 * time.Second
)

var (
	accessResponseClasses = map[string]bool{
		"auth": true, "browse_asset": true, "browse_listing": true, "browse_render": true,
		"file": true, "hub": true, "janus": true, "proxy": true, "sendfile": true,
	}
	accessCacheVerdicts = map[string]bool{
		"off": true, "bypass": true, "miss": true, "hit": true, "coalesced": true,
	}
	accessOutcomes = map[string]bool{
		"upgraded": true, "upstream_aborted": true, "client_canceled": true,
		"write_error": true, "complete": true,
	}
)

var accessPool = caddy.NewUsagePool()

const accessPoolKey = "janus.access"

type accessCounters struct {
	published          atomic.Uint64
	subscriberOverflow atomic.Uint64
	streamOpens        atomic.Uint64
	streamCloses       atomic.Uint64
	writeTimeouts      atomic.Uint64
	invariantFailures  atomic.Uint64
}

type accessBridge struct {
	accountingMu sync.Mutex
	states       map[*accessState]struct{}
	subscribers  uint64
	encoders     uint64
	key          [32]byte
	counters     accessCounters
	logger       *zap.Logger
}

func newAccessBridge(logger *zap.Logger) (*accessBridge, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	b := &accessBridge{states: make(map[*accessState]struct{}), logger: logger.Named("access")}
	if _, err := rand.Read(b.key[:]); err != nil {
		return nil, fmt.Errorf("janus access: generating upstream identity key: %w", err)
	}
	return b, nil
}

func acquireAccessBridge(logger *zap.Logger) (*accessBridge, error) {
	value, _, err := accessPool.LoadOrNew(accessPoolKey, func() (caddy.Destructor, error) {
		return newAccessBridge(logger)
	})
	if err != nil {
		return nil, err
	}
	return value.(*accessBridge), nil
}

func releaseAccessBridge() error {
	_, err := accessPool.Delete(accessPoolKey)
	return err
}

func (b *accessBridge) Destruct() error {
	b.accountingMu.Lock()
	defer b.accountingMu.Unlock()
	if b.subscribers != 0 || len(b.states) != 0 || b.encoders != 0 {
		return fmt.Errorf("janus access: bridge destroyed with %d subscribers, %d registrations, %d encoders",
			b.subscribers, len(b.states), b.encoders)
	}
	return nil
}

func (b *accessBridge) newState() *accessState {
	st := &accessState{bridge: b, subscribers: make(map[*accessSubscriber]struct{})}
	b.accountingMu.Lock()
	b.states[st] = struct{}{}
	b.accountingMu.Unlock()
	return st
}

func (b *accessBridge) upstreamID(path string) string {
	mac := hmac.New(sha256.New, b.key[:])
	_, _ = mac.Write([]byte(path))
	return "worker-" + hex.EncodeToString(mac.Sum(nil)[:8])
}

type accessLine struct {
	sequence uint64
	data     []byte
}

type accessSubscriber struct {
	state          *accessState
	lines          chan *accessLine
	accounted      uint64
	droppedThrough uint64
	done           chan struct{}
	reason         string
	detached       bool
	controller     *http.ResponseController
}

type accessState struct {
	mu          sync.Mutex
	bridge      *accessBridge
	head        uint64
	subscribers map[*accessSubscriber]struct{}
	tombstone   bool
	closeReason string
}

func (st *accessState) observed() bool {
	st.mu.Lock()
	observed := len(st.subscribers) != 0
	st.mu.Unlock()
	return observed
}

// publishUnobserved completes a validated publication without materializing
// an event when no stream can receive it. It returns false if a subscriber
// attached since observed was checked, in which case the caller publishes the
// fully built event normally.
func (st *accessState) publishUnobserved() bool {
	st.mu.Lock()
	if st.tombstone {
		st.mu.Unlock()
		return true
	}
	if len(st.subscribers) != 0 {
		st.mu.Unlock()
		return false
	}
	if st.head == math.MaxUint64 {
		detached := st.tombstoneLocked("sequence_exhausted")
		st.mu.Unlock()
		st.wake(detached)
		st.bridge.logger.Error("janus access sequence exhausted")
		return true
	}
	st.head++
	st.bridge.counters.published.Add(1)
	st.mu.Unlock()
	return true
}

func (st *accessState) publish(build func(uint64) ([]byte, error)) {
	st.mu.Lock()
	if st.tombstone {
		st.mu.Unlock()
		return
	}
	if st.head == math.MaxUint64 {
		detached := st.tombstoneLocked("sequence_exhausted")
		st.mu.Unlock()
		st.wake(detached)
		st.bridge.logger.Error("janus access sequence exhausted")
		return
	}
	st.head++
	seq := st.head
	if len(st.subscribers) == 0 {
		// Sequence and publication accounting are part of the observation
		// contract even without a listener. Avoid constructing and encoding
		// an event that cannot be delivered.
		st.bridge.counters.published.Add(1)
		st.mu.Unlock()
		return
	}
	data, err := build(seq)
	if err != nil {
		for sub := range st.subscribers {
			if sub.droppedThrough < seq {
				sub.droppedThrough = seq
			}
		}
		st.bridge.counters.invariantFailures.Add(1)
		st.mu.Unlock()
		st.bridge.logger.Error("janus access event encoding failed", zap.Uint64("sequence", seq), zap.Error(err))
		return
	}
	line := &accessLine{sequence: seq, data: data}
	for sub := range st.subscribers {
		select {
		case sub.lines <- line:
		default:
			sub.droppedThrough = seq
			st.bridge.counters.subscriberOverflow.Add(1)
		}
	}
	st.bridge.counters.published.Add(1)
	st.mu.Unlock()
}

func (st *accessState) tombstoneLocked(reason string) []*accessSubscriber {
	if st.tombstone {
		return nil
	}
	st.tombstone = true
	st.closeReason = reason
	detached := make([]*accessSubscriber, 0, len(st.subscribers))
	for sub := range st.subscribers {
		sub.detached = true
		sub.reason = reason
		delete(st.subscribers, sub)
		detached = append(detached, sub)
	}
	st.bridge.accountingMu.Lock()
	st.bridge.subscribers -= uint64(len(detached))
	st.bridge.counters.streamCloses.Add(uint64(len(detached)))
	delete(st.bridge.states, st)
	st.bridge.accountingMu.Unlock()
	return detached
}

func (st *accessState) wake(subs []*accessSubscriber) {
	for _, sub := range subs {
		close(sub.done)
	}
}

type accessOwner struct {
	id    string
	name  string
	site  string
	state *accessState
}

type accessFacts struct {
	mu sync.Mutex

	start     time.Time
	requestID string
	owner     accessOwner

	responseClass    string
	cacheVerdict     string
	selected         string
	attempts         uint64
	outcome          string
	upgraded         bool
	mark             *string
	markRejected     bool
	published        atomic.Bool
	completionSet    bool
	completionStatus int64
	completionSize   int64
}

func newAccessFacts(r *http.Request) *accessFacts {
	f := &accessFacts{start: time.Now(), cacheVerdict: "off", outcome: "complete"}
	if repl, ok := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer); ok {
		f.requestID, _ = repl.GetString("http.request.uuid")
	}
	return f
}

func attachAccessFacts(r *http.Request) *accessFacts {
	extra, ok := r.Context().Value(caddyhttp.ExtraLogFieldsCtxKey).(*caddyhttp.ExtraLogFields)
	if !ok {
		return nil
	}
	f := newAccessFacts(r)
	extra.Set(zap.Field{Key: accessFactsKey, Type: zapcore.SkipType, Interface: f})
	return f
}

func accessFactsOf(r *http.Request) *accessFacts {
	_, ok := r.Context().Value(caddyhttp.ExtraLogFieldsCtxKey).(*caddyhttp.ExtraLogFields)
	if !ok {
		return nil
	}
	// ExtraLogFields intentionally exposes mutation but not reads. Janus keeps
	// the pointer in a request context value as well when a request is prepared.
	f, _ := r.Context().Value(accessFactsContextKey{}).(*accessFacts)
	return f
}

type accessFactsContextKey struct{}

func attachAccessFactsRequest(r *http.Request) (*http.Request, *accessFacts) {
	if f := accessFactsOf(r); f != nil {
		return r, f
	}
	f := attachAccessFacts(r)
	if f == nil {
		return r, nil
	}
	return r.WithContext(context.WithValue(r.Context(), accessFactsContextKey{}, f)), f
}

func (f *accessFacts) setOwner(rec AppRecord) {
	if f == nil {
		return
	}
	f.mu.Lock()
	next := accessOwner{id: rec.ID, name: rec.Name, site: rec.siteValue, state: rec.access}
	if f.owner.state != nil && f.owner.state != next.state {
		f.responseClass, f.cacheVerdict, f.selected, f.outcome = "", "off", "", "complete"
		f.attempts, f.mark, f.markRejected, f.upgraded = 0, nil, false, false
	}
	f.owner = next
	f.mu.Unlock()
}

func (f *accessFacts) clearOwner() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.owner = accessOwner{}
	f.responseClass, f.cacheVerdict, f.selected, f.outcome = "", "off", "", "complete"
	f.attempts, f.mark, f.markRejected, f.upgraded = 0, nil, false, false
	f.mu.Unlock()
}

func (f *accessFacts) setClass(class string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.responseClass = class
	f.mu.Unlock()
}

func (f *accessFacts) setCache(verdict string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.cacheVerdict = verdict
	f.mu.Unlock()
}

func (f *accessFacts) attempt(path string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	if f.owner.state == nil {
		f.mu.Unlock()
		return
	}
	f.attempts++
	f.selected = f.owner.state.bridge.upstreamID(path)
	f.mark, f.markRejected = nil, false
	f.mu.Unlock()
}

func (f *accessFacts) setMark(values []string, present bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !present {
		return
	}
	if f.markRejected {
		return
	}
	if len(values) != 1 || values[0] == "" || len(values[0]) > 256 || !utf8.ValidString(values[0]) || f.mark != nil {
		f.mark = nil
		f.markRejected = true
		return
	}
	value := values[0]
	f.mark = &value
}

type accessEvent struct {
	V                int      `json:"v"`
	Type             string   `json:"type"`
	Sequence         string   `json:"sequence"`
	Timestamp        string   `json:"timestamp"`
	RequestID        string   `json:"request_id"`
	AppID            string   `json:"app_id"`
	AppName          string   `json:"app_name"`
	TenantSite       *string  `json:"tenant_site"`
	RequestHost      string   `json:"request_host"`
	ClientIP         string   `json:"client_ip"`
	Method           string   `json:"method"`
	Path             string   `json:"path"`
	Status           *int     `json:"status"`
	DurationSeconds  float64  `json:"duration_seconds"`
	ResponseBytes    string   `json:"response_bytes"`
	MIMEType         *string  `json:"mime_type"`
	ResponseClass    string   `json:"response_class"`
	CacheVerdict     string   `json:"cache_verdict"`
	SelectedUpstream *string  `json:"selected_upstream"`
	RetryCount       uint32   `json:"retry_count"`
	Outcome          string   `json:"outcome"`
	Mark             *string  `json:"mark"`
	TruncatedFields  []string `json:"truncated_fields,omitempty"`
	OmittedFields    []string `json:"omitted_fields,omitempty"`
}

func boundASCII(value string, max int, field string, adjusted *[]string) string {
	if len(value) <= max {
		return value
	}
	*adjusted = append(*adjusted, field)
	return value[:max]
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range []byte(value) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func encodeAccessEvent(event accessEvent) ([]byte, error) {
	sort.Strings(event.TruncatedFields)
	sort.Strings(event.OmittedFields)
	for {
		line, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		line = append(line, '\n')
		if len(line) <= accessLineBytes {
			return line, nil
		}
		over := len(line) - accessLineBytes
		if over >= len(event.Path) {
			return nil, errors.New("fixed access event envelope exceeds 8192 bytes")
		}
		cut := len(event.Path) - over
		for cut > 0 && !utf8.RuneStart(event.Path[cut]) {
			cut--
		}
		event.Path = event.Path[:cut]
		if !slices.Contains(event.TruncatedFields, "path") {
			event.TruncatedFields = append(event.TruncatedFields, "path")
			sort.Strings(event.TruncatedFields)
		}
	}
}

func clientIPOf(r *http.Request) string {
	if value, ok := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string); ok && net.ParseIP(value) != nil {
		return value
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && net.ParseIP(host) != nil {
		return host
	}
	if net.ParseIP(r.RemoteAddr) != nil {
		return r.RemoteAddr
	}
	return ""
}

func responseHeaderField(fields []zapcore.Field) http.Header {
	for _, field := range fields {
		if field.Key != "resp_headers" {
			continue
		}
		switch value := field.Interface.(type) {
		case caddyhttp.LoggableHTTPHeader:
			return value.Header
		case *caddyhttp.LoggableHTTPHeader:
			return value.Header
		}
	}
	return nil
}

func integerField(fields []zapcore.Field, key string) (int64, bool) {
	for _, field := range fields {
		if field.Key == key {
			return field.Integer, true
		}
	}
	return 0, false
}

func buildAccessEvent(f *accessFacts, r *http.Request, entry zapcore.Entry, fields []zapcore.Field, seq uint64) (accessEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := validateAccessEventLocked(f, r, fields); err != nil {
		return accessEvent{}, err
	}
	statusValue, _ := integerField(fields, "status")
	size, _ := integerField(fields, "size")
	status := int(statusValue)
	if f.upgraded {
		status = http.StatusSwitchingProtocols
		size = 0
	} else if r.Method == http.MethodHead {
		size = 0
	}
	var statusPtr *int
	if status == 0 && f.outcome == "complete" {
		status = http.StatusOK
	}
	if status >= 100 && status <= 599 {
		statusPtr = &status
	}
	method, path := r.Method, r.URL.EscapedPath()
	var truncated []string
	method = boundASCII(method, 32, "method", &truncated)
	path = boundASCII(path, 4096, "path", &truncated)
	headers := responseHeaderField(fields)
	var mimeType *string
	var omitted []string
	if values, present := headerValues(headers, "Content-Type"); present {
		if len(values) == 1 {
			value := strings.Trim(values[0], " \t")
			if value != "" && len(value) <= 256 && utf8.ValidString(value) {
				mimeType = &value
			} else {
				omitted = append(omitted, "mime_type")
			}
		} else {
			omitted = append(omitted, "mime_type")
		}
	}
	if f.markRejected {
		omitted = append(omitted, "mark")
	}
	var tenant *string
	if f.owner.site != "" {
		value := f.owner.site
		tenant = &value
	}
	var selected *string
	if f.selected != "" {
		value := f.selected
		selected = &value
	}
	class := f.responseClass
	if class == "" {
		class = "janus"
	}
	retries := uint64(0)
	if f.attempts > 0 {
		retries = f.attempts - 1
	}
	if retries > math.MaxUint32 {
		retries = math.MaxUint32
	}
	duration := time.Since(f.start).Seconds()
	outcome := f.outcome
	if f.upgraded {
		outcome = "upgraded"
	} else if r.Context().Err() != nil {
		outcome = "client_canceled"
	}
	return accessEvent{
		V: 1, Type: "access", Sequence: strconv.FormatUint(seq, 10),
		Timestamp: entry.Time.UTC().Format(time.RFC3339Nano), RequestID: f.requestID,
		AppID: f.owner.id, AppName: f.owner.name, TenantSite: tenant,
		RequestHost: normalizeHostHeader(r.Host), ClientIP: clientIPOf(r),
		Method: method, Path: path, Status: statusPtr, DurationSeconds: duration,
		ResponseBytes: strconv.FormatInt(max(size, 0), 10), MIMEType: mimeType,
		ResponseClass: class, CacheVerdict: f.cacheVerdict, SelectedUpstream: selected,
		RetryCount: uint32(retries), Outcome: outcome, Mark: f.mark,
		TruncatedFields: truncated, OmittedFields: omitted,
	}, nil
}

// validateAccessEvent checks the invariant-bearing fields without allocating
// the event payload. The encoder uses it before publication so malformed
// completions are still reported when no stream is subscribed.
func validateAccessEvent(f *accessFacts, r *http.Request, fields []zapcore.Field) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return validateAccessEventLocked(f, r, fields)
}

func validateAccessEventLocked(f *accessFacts, r *http.Request, fields []zapcore.Field) error {
	if f.owner.state == nil {
		return errors.New("access event has no owner")
	}
	if !validUUID(f.requestID) {
		return errors.New("access event has invalid request id")
	}
	statusValue, _ := integerField(fields, "status")
	if size, _ := integerField(fields, "size"); size < 0 {
		return errors.New("access event has negative recorder size")
	}
	status := int(statusValue)
	if f.upgraded {
		status = http.StatusSwitchingProtocols
	}
	if status == 0 && f.outcome == "complete" {
		status = http.StatusOK
	}
	if status != 0 && (status < 100 || status > 599) {
		return errors.New("access event has invalid status")
	}
	class := f.responseClass
	if class == "" {
		class = "janus"
	}
	if !accessResponseClasses[class] {
		return errors.New("access event has unknown response class")
	}
	if !accessCacheVerdicts[f.cacheVerdict] {
		return errors.New("access event has unknown cache verdict")
	}
	duration := time.Since(f.start).Seconds()
	if duration < 0 || math.IsInf(duration, 0) || math.IsNaN(duration) {
		return errors.New("invalid access duration")
	}
	outcome := f.outcome
	if f.upgraded {
		outcome = "upgraded"
	} else if r.Context().Err() != nil {
		outcome = "client_canceled"
	}
	if !accessOutcomes[outcome] {
		return errors.New("access event has unknown outcome")
	}
	return nil
}

var _ caddy.Destructor = (*accessBridge)(nil)
