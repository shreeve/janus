package janus

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	caddylogging "github.com/caddyserver/caddy/v2/modules/logging"
	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestAccessEncoderCaddyfileGrammar(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		ok   bool
	}{
		{"short", "janus", true},
		{"common options", "janus {\ntime_format rfc3339_nano\nduration_format seconds\ntime_local\n}", true},
		{"argument", "janus nope", false},
		{"unknown", "janus {\nunknown value\n}", false},
		{"wrong arity", "janus {\ntime_format\n}", false},
		{"nested leaf", "janus {\ntime_format rfc3339 {\nvalue nope\n}\n}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var encoder AccessEncoder
			err := encoder.UnmarshalCaddyfile(caddyfile.NewTestDispenser(tc.text))
			if (err == nil) != tc.ok {
				t.Fatalf("UnmarshalCaddyfile() error=%v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestAccessEncoderDurableJSONParityAndPublication(t *testing.T) {
	config := caddylogging.LogEncoderConfig{TimeFormat: "rfc3339_nano", DurationFormat: "seconds"}
	wrapped := zapcore.NewJSONEncoder(config.ZapcoreEncoderConfig())
	encoder := &AccessEncoder{
		LogEncoderConfig: config,
		wrapped:          zapcore.NewJSONEncoder(config.ZapcoreEncoderConfig()),
		logger:           zap.NewNop(),
	}
	request := &http.Request{
		Method: "GET", Host: "cart.example", RemoteAddr: "127.0.0.1:1234",
		URL: &url.URL{Path: "/items", RawPath: "/items"},
	}
	loggable := caddyhttp.LoggableHTTPRequest{Request: request}
	if err := wrapped.AddObject("request", loggable); err != nil {
		t.Fatal(err)
	}
	if err := encoder.AddObject("request", loggable); err != nil {
		t.Fatal(err)
	}

	bridge, err := newAccessBridge(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	state := bridge.newState()
	sub := &accessSubscriber{state: state, lines: make(chan *accessLine, 1), done: make(chan struct{})}
	state.mu.Lock()
	state.subscribers[sub] = struct{}{}
	state.mu.Unlock()
	facts := &accessFacts{
		start: time.Now().Add(-time.Millisecond), requestID: "7a37ea98-38c6-4e23-bdc6-601590d43f04",
		owner:         accessOwner{id: "cart-a1b2c3", name: "cart", state: state},
		responseClass: "proxy", cacheVerdict: "off", outcome: "complete",
	}
	fields := []zapcore.Field{
		zap.Int("status", http.StatusOK),
		zap.Int("size", 12),
		zap.Object("resp_headers", caddyhttp.LoggableHTTPHeader{Header: http.Header{"Content-Type": {"text/plain"}}}),
		{Key: accessFactsKey, Type: zapcore.SkipType, Interface: facts},
	}
	entry := zapcore.Entry{Time: time.Now(), Level: zapcore.InfoLevel, Message: "handled request"}
	want, err := wrapped.EncodeEntry(entry, fields)
	if err != nil {
		t.Fatal(err)
	}
	defer want.Free()
	got, err := encoder.EncodeEntry(entry, fields)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Free()
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatalf("durable JSON changed:\n got %s\nwant %s", got.Bytes(), want.Bytes())
	}
	select {
	case line := <-sub.lines:
		if line.sequence != 1 || len(line.data) == 0 || line.data[len(line.data)-1] != '\n' {
			t.Fatalf("line=%+v", line)
		}
		if !bytes.Contains(line.data, []byte(`"response_class":"proxy"`)) {
			t.Fatalf("access event=%s", line.data)
		}
	default:
		t.Fatal("encoder did not publish")
	}
	// A second configured output receives durable JSON but the request guard
	// prevents another sequence.
	if _, err := encoder.EncodeEntry(entry, fields); err != nil {
		t.Fatal(err)
	}
	if state.head != 1 {
		t.Fatalf("duplicate output advanced head to %d", state.head)
	}
}

func TestAccessEncoderSentinelRules(t *testing.T) {
	newEncoder := func(bridge *accessBridge) *AccessEncoder {
		return &AccessEncoder{
			wrapped: zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
			bridge:  bridge, logger: zap.NewNop(),
		}
	}
	bridge, err := newAccessBridge(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	entry := zapcore.Entry{Time: time.Now(), Level: zapcore.InfoLevel}
	if _, err := newEncoder(bridge).EncodeEntry(entry, nil); err != nil {
		t.Fatal(err)
	}
	if bridge.counters.invariantFailures.Load() != 0 {
		t.Fatal("absent sentinel is not an invariant")
	}
	bad := []zapcore.Field{{Key: accessFactsKey, Type: zapcore.StringType, String: "bad"}}
	if _, err := newEncoder(bridge).EncodeEntry(entry, bad); err != nil {
		t.Fatal(err)
	}
	if bridge.counters.invariantFailures.Load() != 1 {
		t.Fatal("wrong sentinel type was not recorded")
	}
	duplicate := []zapcore.Field{
		{Key: accessFactsKey, Type: zapcore.SkipType, Interface: new(accessFacts)},
		{Key: accessFactsKey, Type: zapcore.SkipType, Interface: new(accessFacts)},
	}
	if _, err := newEncoder(bridge).EncodeEntry(entry, duplicate); err != nil {
		t.Fatal(err)
	}
	if bridge.counters.invariantFailures.Load() != 2 {
		t.Fatal("duplicate sentinel was not recorded")
	}
}

func TestAccessRegistrationLifecycleAndSequence(t *testing.T) {
	bridge, err := newAccessBridge(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	registry := newAppRegistry()
	registry.access = bridge
	rec, err := registry.create("cart", []string{"cart.example"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if rec.access == nil {
		t.Fatal("registration has no access state")
	}
	cloned, err := registry.get(rec.ID)
	if err != nil || cloned.access != rec.access {
		t.Fatalf("clone lost access state: rec=%p clone=%p err=%v", rec.access, cloned.access, err)
	}
	rec.access.publish(func(sequence uint64) ([]byte, error) {
		return []byte(`{"sequence":"` + strconv.FormatUint(sequence, 10) + `"}` + "\n"), nil
	})
	if rec.access.head != 1 {
		t.Fatalf("first sequence=%d", rec.access.head)
	}
	if err := registry.delete(rec.ID); err != nil {
		t.Fatal(err)
	}
	if !rec.access.tombstone || rec.access.closeReason != "registration_deleted" {
		t.Fatalf("deleted state=%+v", rec.access)
	}
	next, err := registry.create("cart", []string{"cart.example"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if next.access == rec.access || next.access.head != 0 {
		t.Fatal("re-registration reused sequence state")
	}
	registry.tombstoneAll("generation_stop")
	if err := bridge.Destruct(); err != nil {
		t.Fatal(err)
	}
}

func TestAccessQueueOverflowAndSequenceExhaustion(t *testing.T) {
	bridge, err := newAccessBridge(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	state := bridge.newState()
	sub := &accessSubscriber{state: state, lines: make(chan *accessLine, 1), done: make(chan struct{})}
	state.mu.Lock()
	state.subscribers[sub] = struct{}{}
	state.mu.Unlock()
	for i := 0; i < 2; i++ {
		state.publish(func(sequence uint64) ([]byte, error) {
			return []byte(strconv.FormatUint(sequence, 10) + "\n"), nil
		})
	}
	if state.head != 2 || sub.droppedThrough != 2 || bridge.counters.subscriberOverflow.Load() != 1 {
		t.Fatalf("head=%d dropped=%d overflows=%d", state.head, sub.droppedThrough, bridge.counters.subscriberOverflow.Load())
	}
	state.mu.Lock()
	state.head = math.MaxUint64
	state.mu.Unlock()
	state.publish(func(uint64) ([]byte, error) { return nil, nil })
	if !state.tombstone || state.closeReason != "sequence_exhausted" {
		t.Fatalf("sequence exhaustion did not tombstone: %+v", state)
	}
}

func TestAccessRipMarkValidationAndTrailerCapture(t *testing.T) {
	facts := &accessFacts{}
	facts.setMark([]string{"header"}, true)
	if facts.mark == nil || *facts.mark != "header" {
		t.Fatalf("header mark=%v", facts.mark)
	}
	trailer := http.Header{"Rip-Mark": {"trailer"}}
	watcher := &bodyWatcher{
		rc: io.NopCloser(strings.NewReader("")), at: new(attemptState),
		trailer: trailer, facts: facts,
	}
	var p [1]byte
	if _, err := watcher.Read(p[:]); err != io.EOF {
		t.Fatalf("Read() error=%v", err)
	}
	if trailer.Get("Rip-Mark") != "" || facts.mark != nil || !facts.markRejected {
		t.Fatalf("ambiguous mark was not omitted: trailer=%v mark=%v rejected=%v", trailer, facts.mark, facts.markRejected)
	}
	facts = &accessFacts{}
	facts.setMark([]string{strings.Repeat("x", 256)}, true)
	if facts.mark == nil {
		t.Fatal("exact 256-byte mark rejected")
	}
	facts = &accessFacts{}
	facts.setMark([]string{strings.Repeat("x", 257)}, true)
	if facts.mark != nil || !facts.markRejected {
		t.Fatal("oversized mark accepted")
	}
	for _, tc := range []struct {
		name   string
		values []string
	}{
		{name: "empty", values: []string{""}},
		{name: "repeated", values: []string{"one", "two"}},
		{name: "oversized", values: []string{strings.Repeat("x", 257)}},
		{name: "invalid UTF-8", values: []string{string([]byte{0xff})}},
	} {
		t.Run(tc.name+" header cannot be resurrected by trailer", func(t *testing.T) {
			facts := accessOutcomeFacts(t)
			facts.setMark(tc.values, true)
			trailer := http.Header{"Rip-Mark": {"valid-trailer"}}
			watcher := &bodyWatcher{
				rc: io.NopCloser(strings.NewReader("")), at: new(attemptState),
				trailer: trailer, facts: facts,
			}
			if _, err := watcher.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("Read() error=%v", err)
			}
			if facts.mark != nil || !facts.markRejected || trailer.Get("Rip-Mark") != "" {
				t.Fatalf("rejected header resurrected: mark=%v rejected=%v trailer=%v",
					facts.mark, facts.markRejected, trailer)
			}
			event, err := buildAccessEvent(facts, httptestRequestWithContext(context.Background()),
				zapcore.Entry{Time: time.Now()}, accessCompletionFields(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if event.Mark != nil || !slices.Contains(event.OmittedFields, "mark") {
				t.Fatalf("event mark=%v omitted=%v", event.Mark, event.OmittedFields)
			}
		})
	}
}

func TestAccessAfterStrictSyntax(t *testing.T) {
	for _, value := range []string{"after=0", "after=1", "after=" + strconv.FormatUint(math.MaxUint64, 10)} {
		if _, err := parseAccessAfter(value); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
	}
	for _, value := range []string{"", "after=", "after=01", "after=-1", "after=1&x=2", "after=%31", "after=1;after=2", "x=1"} {
		if _, err := parseAccessAfter(value); err == nil {
			t.Fatalf("%q accepted", value)
		}
	}
}

func TestAttachAccessFactsRequestIsIdempotent(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://app.test/", nil)
	ctx := context.WithValue(request.Context(), caddyhttp.VarsCtxKey, make(map[string]any))
	ctx = context.WithValue(ctx, caddyhttp.ExtraLogFieldsCtxKey, new(caddyhttp.ExtraLogFields))
	request = request.WithContext(ctx)
	caddyhttp.NewTestReplacer(request)
	firstRequest, first := attachAccessFactsRequest(request)
	secondRequest, second := attachAccessFactsRequest(firstRequest)
	if first == nil || second != first || secondRequest != firstRequest {
		t.Fatalf("facts attachment was not idempotent: first=%p second=%p request=%p/%p",
			first, second, firstRequest, secondRequest)
	}
	if !validUUID(first.requestID) {
		t.Fatalf("request UUID was not materialized: %q", first.requestID)
	}
}

func TestAccessEventBoundsAndAdjustments(t *testing.T) {
	event := accessEvent{
		V: 1, Type: "access", Sequence: "1", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		RequestID: "7a37ea98-38c6-4e23-bdc6-601590d43f04", AppID: "cart-a1b2c3",
		AppName: "cart", RequestHost: "cart.example", ClientIP: "127.0.0.1",
		Method: "GET", Path: "/" + strings.Repeat(`"`, 4095), DurationSeconds: .1,
		ResponseBytes: "0", ResponseClass: "proxy", CacheVerdict: "off", Outcome: "complete",
	}
	line, err := encodeAccessEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	if len(line) > accessLineBytes || line[len(line)-1] != '\n' {
		t.Fatalf("encoded line length=%d", len(line))
	}
	if !bytes.Contains(line, []byte(`"truncated_fields":["path"]`)) {
		t.Fatalf("missing path adjustment: %s", line)
	}
}

func TestAccessMIMETrimsOnlyHTTPWhitespace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		want    string
		omitted bool
	}{
		{name: "space and tab", input: " \ttext/plain\t ", want: "text/plain"},
		{name: "non-breaking spaces preserved", input: "\u00a0text/plain\u00a0", want: "\u00a0text/plain\u00a0"},
		{name: "em spaces preserved", input: "\u2003text/plain\u2003", want: "\u2003text/plain\u2003"},
		{name: "only HTTP whitespace rejects", input: " \t ", omitted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := accessOutcomeFacts(t)
			headers := http.Header{"Content-Type": {tc.input}}
			fields := []zapcore.Field{
				zap.Int("status", http.StatusOK),
				zap.Int("size", 0),
				zap.Object("resp_headers", caddyhttp.LoggableHTTPHeader{Header: headers}),
			}
			event, err := buildAccessEvent(facts, httptestRequestWithContext(context.Background()),
				zapcore.Entry{Time: time.Now()}, fields, 1)
			if err != nil {
				t.Fatal(err)
			}
			if tc.omitted {
				if event.MIMEType != nil || !slices.Contains(event.OmittedFields, "mime_type") {
					t.Fatalf("mime=%v omitted=%v", event.MIMEType, event.OmittedFields)
				}
				return
			}
			if event.MIMEType == nil || *event.MIMEType != tc.want ||
				slices.Contains(event.OmittedFields, "mime_type") {
				t.Fatalf("mime=%v omitted=%v, want %q", event.MIMEType, event.OmittedFields, tc.want)
			}
		})
	}
}

func TestAccessRegistryBridgeBindingConcurrent(t *testing.T) {
	bridge, err := newAccessBridge(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	registry := newAppRegistry()
	if err := registry.bindAccess(bridge); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				if err := registry.bindAccess(bridge); err != nil {
					t.Errorf("bindAccess(): %v", err)
					return
				}
				name := fmt.Sprintf("app%d-%d", worker, n)
				host := fmt.Sprintf("%s.example", name)
				rec, err := registry.create(name, []string{host}, "")
				if err != nil {
					t.Errorf("create(%q): %v", name, err)
					return
				}
				if rec.access == nil || rec.access.bridge != bridge {
					t.Errorf("registration received wrong access state: %+v", rec.access)
					return
				}
			}
		}()
	}
	wg.Wait()
	registry.tombstoneAll("generation_stop")
	if err := bridge.Destruct(); err != nil {
		t.Fatal(err)
	}
	other, err := newAccessBridge(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.bindAccess(other); err == nil {
		t.Fatal("different pooled bridge was accepted")
	}
}

type accessErrorReadCloser struct{ err error }

func (r accessErrorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (accessErrorReadCloser) Close() error               { return nil }

func TestAccessOutcomeClientCancellationVersusUpstreamAbort(t *testing.T) {
	bodyErr := errors.New("worker body aborted")
	for _, tc := range []struct {
		name        string
		cancelFirst bool
		cancelAfter bool
		want        string
	}{
		{name: "genuine upstream abort", want: "upstream_aborted"},
		{name: "client canceled before read failure", cancelFirst: true, want: "client_canceled"},
		{name: "client canceled before completion", cancelAfter: true, want: "client_canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelFirst {
				cancel()
			}
			facts := accessOutcomeFacts(t)
			watcher := &bodyWatcher{
				rc: accessErrorReadCloser{err: bodyErr}, at: new(attemptState),
				trailer: make(http.Header), facts: facts, ctx: ctx,
			}
			if _, err := watcher.Read(make([]byte, 1)); !errors.Is(err, bodyErr) {
				t.Fatalf("Read() error=%v", err)
			}
			if tc.cancelAfter {
				cancel()
			}
			request := httptestRequestWithContext(ctx)
			event, err := buildAccessEvent(facts, request, zapcore.Entry{Time: time.Now()}, accessCompletionFields(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if event.Outcome != tc.want {
				t.Fatalf("outcome=%q, want %q", event.Outcome, tc.want)
			}
		})
	}
}

func accessOutcomeFacts(t *testing.T) *accessFacts {
	t.Helper()
	bridge, err := newAccessBridge(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	state := bridge.newState()
	return &accessFacts{
		start: time.Now(), requestID: "7a37ea98-38c6-4e23-bdc6-601590d43f04",
		owner:         accessOwner{id: "cart-a1b2c3", name: "cart", state: state},
		responseClass: "proxy", cacheVerdict: "off", outcome: "complete",
	}
}

func httptestRequestWithContext(ctx context.Context) *http.Request {
	request := &http.Request{
		Method: http.MethodGet, Host: "cart.example", RemoteAddr: "127.0.0.1:1234",
		URL: &url.URL{Path: "/items", RawPath: "/items"},
	}
	return request.WithContext(ctx)
}

func accessCompletionFields() []zapcore.Field {
	return []zapcore.Field{
		zap.Int("status", http.StatusOK),
		zap.Int("size", 12),
		zap.Object("resp_headers", caddyhttp.LoggableHTTPHeader{Header: make(http.Header)}),
	}
}

type accessTestWriter struct {
	mu        sync.Mutex
	header    http.Header
	body      bytes.Buffer
	status    int
	writeErr  error
	flushes   int
	deadlines []time.Time
	onFlush   func(int)
}

func newAccessTestWriter() *accessTestWriter {
	return &accessTestWriter{header: make(http.Header)}
}

func (w *accessTestWriter) Header() http.Header { return w.header }

func (w *accessTestWriter) WriteHeader(status int) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = status
	}
	w.mu.Unlock()
}

func (w *accessTestWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.body.Write(p)
}

func (w *accessTestWriter) Flush() {
	w.mu.Lock()
	w.flushes++
	count, callback := w.flushes, w.onFlush
	w.mu.Unlock()
	if callback != nil {
		callback(count)
	}
}

func (w *accessTestWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadlines = append(w.deadlines, deadline)
	w.mu.Unlock()
	return nil
}

func (w *accessTestWriter) snapshot() (int, string, int, []time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status, w.body.String(), w.flushes, append([]time.Time(nil), w.deadlines...)
}

type accessOptionalWriter struct {
	*accessTestWriter
	pushed string
	conn   net.Conn
	peer   net.Conn
}

func newAccessOptionalWriter() *accessOptionalWriter {
	conn, peer := net.Pipe()
	return &accessOptionalWriter{accessTestWriter: newAccessTestWriter(), conn: conn, peer: peer}
}

func (w *accessOptionalWriter) Push(target string, _ *http.PushOptions) error {
	w.pushed = target
	return nil
}

func (w *accessOptionalWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func newAccessHandlerApp(t *testing.T) (*App, AppRecord) {
	t.Helper()
	bridge, err := newAccessBridge(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	registry := newAppRegistry()
	if err := registry.bindAccess(bridge); err != nil {
		t.Fatal(err)
	}
	rec, err := registry.create("cart", []string{"cart.example"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return &App{
		access: bridge, appsReg: registry,
		accessStreams: make(map[*accessSubscriber]struct{}),
	}, rec
}

func TestAccessStreamExactFramingAndInitialGap(t *testing.T) {
	app, rec := newAccessHandlerApp(t)
	rec.access.publish(func(sequence uint64) ([]byte, error) {
		return []byte(fmt.Sprintf("{\"v\":1,\"type\":\"access\",\"sequence\":%q}\n", strconv.FormatUint(sequence, 10))), nil
	})
	rec.access.publish(func(sequence uint64) ([]byte, error) {
		return []byte(fmt.Sprintf("{\"v\":1,\"type\":\"access\",\"sequence\":%q}\n", strconv.FormatUint(sequence, 10))), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	writer := newAccessTestWriter()
	writer.onFlush = func(count int) {
		if count == 2 {
			cancel()
		}
	}
	request := httptestRequestWithContext(ctx)
	request.URL = &url.URL{Path: "/1.0/apps/" + rec.ID + "/access", RawQuery: "after=0"}
	request.SetPathValue("id", rec.ID)
	app.handleAccessStream(writer, request)

	status, body, flushes, _ := writer.snapshot()
	if status != http.StatusOK || flushes != 2 {
		t.Fatalf("status=%d flushes=%d body=%q", status, flushes, body)
	}
	lines := strings.SplitAfter(body, "\n")
	if len(lines) != 3 || lines[2] != "" {
		t.Fatalf("stream framing=%q", body)
	}
	wantHello := fmt.Sprintf(
		"{\"v\":1,\"type\":\"hello\",\"app_id\":%q,\"app_name\":\"cart\",\"after\":\"0\",\"head\":\"2\",\"heartbeat_seconds\":15,\"max_line_bytes\":8192}\n",
		rec.ID)
	wantGap := fmt.Sprintf(
		"{\"v\":1,\"type\":\"gap\",\"app_id\":%q,\"from\":\"1\",\"through\":\"2\",\"head\":\"2\",\"reason\":\"no_replay\"}\n",
		rec.ID)
	if lines[0] != wantHello || lines[1] != wantGap || strings.Contains(body, "\r") {
		t.Fatalf("stream bytes:\n got %q\nwant %q", body, wantHello+wantGap)
	}
}

func TestAccessAdmissionPrecedenceAndBodyShapes(t *testing.T) {
	app, _ := newAccessHandlerApp(t)
	for _, tc := range []struct {
		name          string
		method        string
		query         string
		body          io.Reader
		contentLength int64
		transfer      []string
		want          int
	}{
		{name: "method before body query and app", method: http.MethodPost, query: "bad", body: strings.NewReader("x"), want: http.StatusMethodNotAllowed},
		{name: "genuine GET body before query and app", method: http.MethodGet, query: "bad", body: strings.NewReader("x"), contentLength: 1, want: http.StatusBadRequest},
		{name: "unknown length GET body", method: http.MethodGet, query: "after=0", body: strings.NewReader(""), contentLength: -1, want: http.StatusBadRequest},
		{name: "chunked empty GET body", method: http.MethodGet, query: "after=0", body: strings.NewReader(""), transfer: []string{"chunked"}, want: http.StatusBadRequest},
		{name: "query before app", method: http.MethodGet, query: "after=01", want: http.StatusBadRequest},
		{name: "app after valid request shape", method: http.MethodGet, query: "after=0", want: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequest(tc.method, "http://control/1.0/apps/missing/access?"+tc.query, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if tc.contentLength != 0 {
				request.ContentLength = tc.contentLength
			}
			request.TransferEncoding = tc.transfer
			request.SetPathValue("id", "missing")
			writer := newAccessTestWriter()
			app.handleAccessStream(writer, request)
			status, _, _, _ := writer.snapshot()
			if status != tc.want {
				t.Fatalf("status=%d, want %d", status, tc.want)
			}
			if tc.want == http.StatusMethodNotAllowed && writer.Header().Get("Allow") != "GET" {
				t.Fatalf("Allow=%q", writer.Header().Get("Allow"))
			}
		})
	}
}

func TestAccessWriterOptionalOperationsAndOutcomePrecedence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	request := httptestRequestWithContext(ctx)
	facts := &accessFacts{outcome: "upstream_aborted"}
	base := newAccessOptionalWriter()
	defer base.conn.Close()
	defer base.peer.Close()
	writer := &accessResponseWriter{ResponseWriter: base, request: request, facts: facts}
	if writer.Unwrap() != base {
		t.Fatal("Unwrap did not preserve the underlying writer")
	}
	if err := http.NewResponseController(writer).Flush(); err != nil {
		t.Fatalf("Flush through Unwrap: %v", err)
	}
	if err := writer.Push("/asset.css", nil); err != nil || base.pushed != "/asset.css" {
		t.Fatalf("Push() error=%v target=%q", err, base.pushed)
	}
	conn, _, err := writer.Hijack()
	if err != nil || conn != base.conn {
		t.Fatalf("Hijack() conn=%v error=%v", conn, err)
	}
	if _, err := writer.ReadFrom(strings.NewReader("body")); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	writer.record(errors.New("write failed"))
	if facts.outcome != "upstream_aborted" {
		t.Fatalf("live write error displaced upstream abort: %q", facts.outcome)
	}
	cancel()
	writer.record(errors.New("client disconnected"))
	if facts.outcome != "client_canceled" {
		t.Fatalf("cancellation did not win: %q", facts.outcome)
	}
	unsupported := &accessResponseWriter{
		ResponseWriter: newAccessTestWriter(), request: request, facts: facts,
	}
	if err := unsupported.Push("/asset.css", nil); !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("unsupported Push() error=%v", err)
	}
}

func TestAccessOverflowGapPositionsAndTrailingLoss(t *testing.T) {
	app, rec := newAccessHandlerApp(t)
	sub := &accessSubscriber{
		state: rec.access, lines: make(chan *accessLine, 4),
		accounted: 0, droppedThrough: 2, done: make(chan struct{}),
	}
	writer := newAccessTestWriter()
	controller := http.NewResponseController(writer)
	line := &accessLine{sequence: 3, data: []byte("{\"sequence\":\"3\"}\n")}
	if !app.writeQueuedAccess(controller, writer, sub, line, rec.ID) {
		t.Fatal("middle overflow gap failed")
	}
	sub.droppedThrough = 5
	if !app.writeTrailingGap(controller, writer, sub, rec.ID) {
		t.Fatal("trailing overflow gap failed")
	}
	_, body, _, _ := writer.snapshot()
	want := string(accessGapLine(rec.ID, 1, 2, 0, "overflow")) +
		string(line.data) + string(accessGapLine(rec.ID, 4, 5, 0, "overflow"))
	if body != want {
		t.Fatalf("gap positions:\n got %q\nwant %q", body, want)
	}
}

func TestAccessPrivacyAndRipMarkScrubbing(t *testing.T) {
	facts := accessOutcomeFacts(t)
	facts.attempt("/private/run/cart.sock")
	request := httptestRequestWithContext(context.Background())
	request.URL = &url.URL{Path: "/items", RawPath: "/items", RawQuery: "token=secret"}
	request.Header = http.Header{
		"Authorization": {"Bearer secret"}, "Cookie": {"session=secret"},
	}
	mark := "safe"
	facts.mark = &mark
	event, err := buildAccessEvent(facts, request, zapcore.Entry{Time: time.Now()}, accessCompletionFields(), 1)
	if err != nil {
		t.Fatal(err)
	}
	line, err := encodeAccessEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"token=secret", "Bearer secret", "session=secret", "/private/run/cart.sock"} {
		if bytes.Contains(line, []byte(secret)) {
			t.Fatalf("stream leaked %q: %s", secret, line)
		}
	}
	if event.Path != "/items" || event.SelectedUpstream == nil || !strings.HasPrefix(*event.SelectedUpstream, "worker-") {
		t.Fatalf("bounded privacy fields=%+v", event)
	}

	header := http.Header{
		"Rip-Mark": {"hidden"},
		"Trailer":  {"Rip-Mark, ETag"},
	}
	header.Del("Rip-Mark")
	scrubTrailerDeclaration(header, "Rip-Mark")
	if header.Get("Rip-Mark") != "" || header.Get("Trailer") != "ETag" {
		t.Fatalf("mark scrub failed: %v", header)
	}
}

func TestAccessEncoderNoOwnerAndPublicationFailurePolicy(t *testing.T) {
	bridge, err := newAccessBridge(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	encoder := &AccessEncoder{
		wrapped: zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		bridge:  bridge, logger: zap.NewNop(),
	}
	request := httptestRequestWithContext(context.Background())
	loggable := caddyhttp.LoggableHTTPRequest{Request: request}
	if err := encoder.AddObject("request", loggable); err != nil {
		t.Fatal(err)
	}
	facts := &accessFacts{requestID: "7a37ea98-38c6-4e23-bdc6-601590d43f04"}
	fields := append(accessCompletionFields(), zap.Field{Key: accessFactsKey, Type: zapcore.SkipType, Interface: facts})
	if _, err := encoder.EncodeEntry(zapcore.Entry{Time: time.Now()}, fields); err != nil {
		t.Fatal(err)
	}
	if bridge.counters.published.Load() != 0 || bridge.counters.invariantFailures.Load() != 0 {
		t.Fatalf("no-owner entry changed counters: %+v", bridge.counters)
	}

	state := bridge.newState()
	facts.owner = accessOwner{id: "cart-a1b2c3", name: "cart", state: state}
	facts.responseClass, facts.cacheVerdict, facts.outcome = "proxy", "off", "complete"
	facts.start = time.Now()
	facts.published.Store(false)
	facts.requestID = "not-a-uuid"
	if _, err := encoder.EncodeEntry(zapcore.Entry{Time: time.Now()}, fields); err != nil {
		t.Fatal(err)
	}
	if state.head != 0 || bridge.counters.invariantFailures.Load() != 1 {
		t.Fatalf("publication failure head=%d invariants=%d", state.head, bridge.counters.invariantFailures.Load())
	}
}

func TestAccessBlockedWriteDeadlineAndGenerationStop(t *testing.T) {
	app, rec := newAccessHandlerApp(t)
	writer := newAccessTestWriter()
	writer.writeErr = os.ErrDeadlineExceeded
	if app.writeAccessLine(http.NewResponseController(writer), writer, []byte("{}\n")) {
		t.Fatal("deadline write unexpectedly succeeded")
	}
	if app.access.counters.writeTimeouts.Load() != 1 {
		t.Fatalf("write_timeouts=%d", app.access.counters.writeTimeouts.Load())
	}
	_, _, _, deadlines := writer.snapshot()
	if len(deadlines) != 1 || deadlines[0].Before(time.Now().Add(time.Second)) {
		t.Fatalf("ordinary deadline=%v", deadlines)
	}

	sub := &accessSubscriber{
		state: rec.access, lines: make(chan *accessLine, 1),
		done: make(chan struct{}), controller: http.NewResponseController(newAccessTestWriter()),
	}
	rec.access.mu.Lock()
	rec.access.subscribers[sub] = struct{}{}
	rec.access.mu.Unlock()
	app.access.accountingMu.Lock()
	app.access.subscribers++
	app.access.accountingMu.Unlock()
	app.accessStreams[sub] = struct{}{}
	app.stopAccessStreams()
	if !sub.detached || sub.reason != "generation_stop" {
		t.Fatalf("generation stop did not detach stream: %+v", sub)
	}
	select {
	case <-sub.done:
	default:
		t.Fatal("generation stop did not wake stream")
	}
}

func TestAccessReloadRetainsSequenceAndStopsOldGeneration(t *testing.T) {
	bridge, err := newAccessBridge(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	registry := newAppRegistry()
	if err := registry.bindAccess(bridge); err != nil {
		t.Fatal(err)
	}
	rec, err := registry.create("cart", []string{"cart.example"}, "")
	if err != nil {
		t.Fatal(err)
	}
	rec.access.publish(func(sequence uint64) ([]byte, error) {
		return []byte(strconv.FormatUint(sequence, 10) + "\n"), nil
	})
	old := &App{
		access: bridge, appsReg: registry,
		accessStreams: make(map[*accessSubscriber]struct{}),
	}
	replacement := &App{
		access: bridge, appsReg: registry,
		accessStreams: make(map[*accessSubscriber]struct{}),
	}
	if err := registry.bindAccess(replacement.access); err != nil {
		t.Fatal(err)
	}
	sub := &accessSubscriber{
		state: rec.access, lines: make(chan *accessLine, 1),
		done: make(chan struct{}), controller: http.NewResponseController(newAccessTestWriter()),
	}
	rec.access.mu.Lock()
	rec.access.subscribers[sub] = struct{}{}
	rec.access.mu.Unlock()
	bridge.accountingMu.Lock()
	bridge.subscribers++
	bridge.accountingMu.Unlock()
	old.accessStreams[sub] = struct{}{}

	old.stopAccessStreams()
	if !sub.detached || sub.reason != "generation_stop" {
		t.Fatalf("old generation stream survived stop: %+v", sub)
	}
	got, err := replacement.appsReg.get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.access != rec.access || got.access.head != 1 || got.access.tombstone {
		t.Fatalf("reload lost pooled registration state: %+v", got.access)
	}
	got.access.publish(func(sequence uint64) ([]byte, error) {
		return []byte(strconv.FormatUint(sequence, 10) + "\n"), nil
	})
	if got.access.head != 2 {
		t.Fatalf("replacement sequence=%d, want 2", got.access.head)
	}
}

func TestAccessLifecycleRetainsStateAndRejectsLateCompletion(t *testing.T) {
	bridge, err := newAccessBridge(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	registry := newAppRegistry()
	if err := registry.bindAccess(bridge); err != nil {
		t.Fatal(err)
	}
	rec, err := registry.create("cart", []string{"cart.example"}, "")
	if err != nil {
		t.Fatal(err)
	}
	state := rec.access
	name := "shop"
	if patched, err := registry.patch(rec.ID, &name, nil, nil); err != nil || patched.access != state {
		t.Fatalf("PATCH access state=%p want=%p err=%v", patched.access, state, err)
	}
	if updated, err := registry.setUpstreams(rec.ID, []Upstream{{Path: "/run/cart.sock"}}); err != nil || updated.access != state {
		t.Fatalf("upstream PUT access state=%p want=%p err=%v", updated.access, state, err)
	}
	if err := registry.delete(rec.ID); err != nil {
		t.Fatal(err)
	}
	state.publish(func(sequence uint64) ([]byte, error) {
		return []byte(strconv.FormatUint(sequence, 10)), nil
	})
	if state.head != 0 {
		t.Fatalf("late completion advanced tombstoned head to %d", state.head)
	}
	next, err := registry.create("cart", []string{"cart.example"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if next.access == state || next.access.head != 0 {
		t.Fatal("re-registration reused tombstoned access state")
	}
}

func TestAccessStatusExactShapeAndRedaction(t *testing.T) {
	app, _ := newAccessHandlerApp(t)
	writer := newAccessTestWriter()
	app.handleAccessStatus(writer, httptestRequestWithContext(context.Background()))
	_, body, _, _ := writer.snapshot()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 5 {
		t.Fatalf("status top-level shape=%v", decoded)
	}
	for _, secret := range []string{"path", "mark", "host", "client_ip", "upstream"} {
		if strings.Contains(body, secret) {
			t.Fatalf("status leaked %q: %s", secret, body)
		}
	}
}

type accessBenchmarkMode struct {
	name        string
	states      int
	subscribers int
	overflow    bool
}

func BenchmarkAccessEncoderBridge(b *testing.B) {
	config := caddylogging.LogEncoderConfig{TimeFormat: "rfc3339_nano", DurationFormat: "seconds"}
	entry := zapcore.Entry{Time: time.Unix(1_753_000_000, 123456789), Level: zapcore.InfoLevel, Message: "handled request"}
	request := httptestRequestWithContext(context.Background())
	request.URL = &url.URL{Path: "/products/blue cup", RawPath: "/products/blue%20cup"}
	baseFields := accessCompletionFields()

	b.Run("json-baseline", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			encoder := zapcore.NewJSONEncoder(config.ZapcoreEncoderConfig())
			if err := encoder.AddObject("request", caddyhttp.LoggableHTTPRequest{Request: request}); err != nil {
				b.Fatal(err)
			}
			buf, err := encoder.EncodeEntry(entry, baseFields)
			if err != nil {
				b.Fatal(err)
			}
			buf.Free()
		}
	})

	modes := []accessBenchmarkMode{
		{name: "no-subscribers", states: 1},
		{name: "one-draining-subscriber", states: 1, subscribers: 1},
		{name: "four-subscriber-fanout", states: 1, subscribers: 4},
		{name: "blocked-subscriber-overflow", states: 1, subscribers: 1, overflow: true},
		{name: "process-cap-across-registrations", states: accessSubscribersProcess, subscribers: accessSubscribersProcess},
	}
	for _, mode := range modes {
		b.Run(mode.name, func(b *testing.B) {
			bridge, err := newAccessBridge(zap.NewNop())
			if err != nil {
				b.Fatal(err)
			}
			states := make([]*accessState, mode.states)
			var drains sync.WaitGroup
			for i := range states {
				states[i] = bridge.newState()
				if i >= mode.subscribers {
					continue
				}
				sub := &accessSubscriber{
					state: states[i], lines: make(chan *accessLine, accessQueueEvents),
					done: make(chan struct{}),
				}
				states[i].subscribers[sub] = struct{}{}
				if mode.overflow {
					line := &accessLine{data: []byte("{}\n")}
					for range accessQueueEvents {
						sub.lines <- line
					}
					continue
				}
				drains.Add(1)
				go func(lines <-chan *accessLine) {
					defer drains.Done()
					for range lines {
					}
				}(sub.lines)
			}
			if mode.states == 1 && mode.subscribers > 1 {
				for i := 1; i < mode.subscribers; i++ {
					sub := &accessSubscriber{
						state: states[0], lines: make(chan *accessLine, accessQueueEvents),
						done: make(chan struct{}),
					}
					states[0].subscribers[sub] = struct{}{}
					drains.Add(1)
					go func(lines <-chan *accessLine) {
						defer drains.Done()
						for range lines {
						}
					}(sub.lines)
				}
			}
			b.Cleanup(func() {
				for _, state := range states {
					for sub := range state.subscribers {
						close(sub.lines)
					}
				}
				drains.Wait()
			})
			b.ReportAllocs()
			b.ReportMetric(float64(mode.subscribers), "subscribers")
			var iteration uint64
			for b.Loop() {
				state := states[iteration%uint64(len(states))]
				iteration++
				facts := accessBenchmarkFacts(state, "proxy", false)
				fields := append(append([]zapcore.Field(nil), baseFields...),
					zap.Field{Key: accessFactsKey, Type: zapcore.SkipType, Interface: facts})
				encoder := &AccessEncoder{
					LogEncoderConfig: config,
					wrapped:          zapcore.NewJSONEncoder(config.ZapcoreEncoderConfig()),
					bridge:           bridge,
					logger:           zap.NewNop(),
				}
				if err := encoder.AddObject("request", caddyhttp.LoggableHTTPRequest{Request: request}); err != nil {
					b.Fatal(err)
				}
				buf, err := encoder.EncodeEntry(entry, fields)
				if err != nil {
					b.Fatal(err)
				}
				buf.Free()
			}
		})
	}
}

func BenchmarkAccessRepresentativeCompletion(b *testing.B) {
	for _, workload := range []struct {
		name          string
		class         string
		upgraded      bool
		status        int
		size          int
		contentType   string
		contentEncode string
	}{
		{name: "registered-file", class: "file", status: http.StatusOK, size: 64 << 10, contentType: "application/octet-stream"},
		{name: "sendfile", class: "sendfile", status: http.StatusPartialContent, size: 32 << 10, contentType: "application/octet-stream"},
		{name: "gzip", class: "proxy", status: http.StatusOK, size: 8192, contentType: "text/html; charset=utf-8", contentEncode: "gzip"},
		{name: "zstd", class: "proxy", status: http.StatusOK, size: 7424, contentType: "text/html; charset=utf-8", contentEncode: "zstd"},
		{name: "websocket-101", class: "hub", upgraded: true, status: http.StatusSwitchingProtocols},
	} {
		b.Run(workload.name, func(b *testing.B) {
			config := caddylogging.LogEncoderConfig{TimeFormat: "rfc3339_nano", DurationFormat: "seconds"}
			entry := zapcore.Entry{Time: time.Unix(1_753_000_000, 123456789), Level: zapcore.InfoLevel, Message: "handled request"}
			request := httptestRequestWithContext(context.Background())
			headers := make(http.Header)
			headers.Set("Content-Type", workload.contentType)
			if workload.contentEncode != "" {
				headers.Set("Content-Encoding", workload.contentEncode)
			}
			baseFields := []zapcore.Field{
				zap.Int("status", workload.status),
				zap.Int("size", workload.size),
				zap.Object("resp_headers", caddyhttp.LoggableHTTPHeader{Header: headers}),
			}
			bridge, err := newAccessBridge(zap.NewNop())
			if err != nil {
				b.Fatal(err)
			}
			state := bridge.newState()
			b.ReportAllocs()
			for b.Loop() {
				facts := accessBenchmarkFacts(state, workload.class, workload.upgraded)
				fields := append(append([]zapcore.Field(nil), baseFields...),
					zap.Field{Key: accessFactsKey, Type: zapcore.SkipType, Interface: facts})
				encoder := &AccessEncoder{
					LogEncoderConfig: config,
					wrapped:          zapcore.NewJSONEncoder(config.ZapcoreEncoderConfig()),
					bridge:           bridge,
					logger:           zap.NewNop(),
				}
				if err := encoder.AddObject("request", caddyhttp.LoggableHTTPRequest{Request: request}); err != nil {
					b.Fatal(err)
				}
				buf, err := encoder.EncodeEntry(entry, fields)
				if err != nil {
					b.Fatal(err)
				}
				buf.Free()
			}
		})
	}
}

func accessBenchmarkFacts(state *accessState, class string, upgraded bool) *accessFacts {
	return &accessFacts{
		start: time.Now(), requestID: "7a37ea98-38c6-4e23-bdc6-601590d43f04",
		owner:         accessOwner{id: "cart-a1b2c3", name: "cart", site: "alice", state: state},
		responseClass: class, cacheVerdict: "off", outcome: "complete", upgraded: upgraded,
	}
}

func BenchmarkAccessFilesPath(b *testing.B) {
	root := b.TempDir()
	payload := bytes.Repeat([]byte("f"), 64<<10)
	if err := os.WriteFile(filepath.Join(root, "asset.bin"), payload, 0o644); err != nil {
		b.Fatal(err)
	}
	bridge, registry := accessBenchmarkRegistry(b)
	policy := &FilesPolicy{
		Roots: []FilesRoot{{Path: root, Cache: filesCacheRevalidate}},
		Shell: filepath.Join(root, "asset.bin"),
	}
	rec, err := registry.createWithPolicy("files", []string{"files.test"}, nil, policy, "")
	if err != nil {
		b.Fatal(err)
	}
	enabled := true
	handler := &Handler{dp: newDataPlane(registry, zap.NewNop()), Files: &enabled}
	benchmarkAccessPathModes(b, bridge, func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "http://files.test/asset.bin", nil)
	}, func(w http.ResponseWriter, r *http.Request) error {
		return handler.ServeHTTP(w, r, nil)
	}, func(withAccess bool, facts *accessFacts, recorder caddyhttp.ResponseRecorder) error {
		if recorder.Status() != http.StatusOK || recorder.Size() != len(payload) {
			return fmt.Errorf("file status=%d size=%d", recorder.Status(), recorder.Size())
		}
		if withAccess && (facts.owner.state != rec.access || facts.responseClass != "file") {
			return fmt.Errorf("file facts=%+v class=%q", facts.owner, facts.responseClass)
		}
		return nil
	})
}

func BenchmarkAccessSendfilePath(b *testing.B) {
	root := b.TempDir()
	name := filepath.Join(root, "asset.bin")
	payload := bytes.Repeat([]byte("s"), 1<<20)
	if err := os.WriteFile(name, payload, 0o644); err != nil {
		b.Fatal(err)
	}
	socket := startUnixHTTP(b, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(sendfileHeader, name)
		w.WriteHeader(http.StatusOK)
	}))
	bridge, registry := accessBenchmarkRegistry(b)
	rec, err := registry.create("sendfile", []string{"sendfile.test"}, "")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := registry.setUpstreams(rec.ID, []Upstream{{Path: socket}}); err != nil {
		b.Fatal(err)
	}
	handler := &Handler{dp: newDataPlane(registry, zap.NewNop())}
	benchmarkAccessPathModes(b, bridge, func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "http://sendfile.test/asset.bin", nil)
	}, func(w http.ResponseWriter, r *http.Request) error {
		return handler.ServeHTTP(w, r, nil)
	}, func(withAccess bool, facts *accessFacts, recorder caddyhttp.ResponseRecorder) error {
		if recorder.Status() != http.StatusOK || recorder.Size() != len(payload) {
			return fmt.Errorf("sendfile status=%d size=%d", recorder.Status(), recorder.Size())
		}
		if withAccess && (facts.owner.state != rec.access || facts.responseClass != "sendfile") {
			return fmt.Errorf("sendfile facts=%+v class=%q", facts.owner, facts.responseClass)
		}
		return nil
	})
}

func BenchmarkAccessGzipProxyPath(b *testing.B) {
	payload := bytes.Repeat([]byte("compressible access benchmark payload\n"), 2048)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(payload); err != nil {
		b.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		b.Fatal(err)
	}
	socket := startUnixHTTP(b, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(compressed.Len()))
		_, _ = w.Write(compressed.Bytes())
	}))
	bridge, registry := accessBenchmarkRegistry(b)
	rec, err := registry.create("gzip", []string{"gzip.test"}, "")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := registry.setUpstreams(rec.ID, []Upstream{{Path: socket}}); err != nil {
		b.Fatal(err)
	}
	handler := &Handler{dp: newDataPlane(registry, zap.NewNop())}
	benchmarkAccessPathModes(b, bridge, func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "http://gzip.test/data", nil)
	}, func(w http.ResponseWriter, r *http.Request) error {
		return handler.ServeHTTP(w, r, nil)
	}, func(withAccess bool, facts *accessFacts, recorder caddyhttp.ResponseRecorder) error {
		if recorder.Status() != http.StatusOK || recorder.Size() != len(payload) {
			return fmt.Errorf("gzip status=%d size=%d", recorder.Status(), recorder.Size())
		}
		if withAccess && (facts.owner.state != rec.access || facts.responseClass != "proxy") {
			return fmt.Errorf("gzip facts=%+v class=%q", facts.owner, facts.responseClass)
		}
		return nil
	})
}

func BenchmarkAccessZstdProxyPath(b *testing.B) {
	payload := bytes.Repeat([]byte("compressible access benchmark payload\n"), 2048)
	writer, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		b.Fatal(err)
	}
	compressed := writer.EncodeAll(payload, nil)
	writer.Close()
	socket := startUnixHTTP(b, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Encoding", "zstd")
		w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
		_, _ = w.Write(compressed)
	}))
	bridge, registry := accessBenchmarkRegistry(b)
	rec, err := registry.create("zstd", []string{"zstd.test"}, "")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := registry.setUpstreams(rec.ID, []Upstream{{Path: socket}}); err != nil {
		b.Fatal(err)
	}
	handler := &Handler{dp: newDataPlane(registry, zap.NewNop())}
	benchmarkAccessPathModes(b, bridge, func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "http://zstd.test/data", nil)
	}, func(w http.ResponseWriter, r *http.Request) error {
		return handler.ServeHTTP(w, r, nil)
	}, func(withAccess bool, facts *accessFacts, recorder caddyhttp.ResponseRecorder) error {
		if recorder.Status() != http.StatusOK || recorder.Size() != len(compressed) {
			return fmt.Errorf("zstd status=%d size=%d", recorder.Status(), recorder.Size())
		}
		if withAccess && (facts.owner.state != rec.access || facts.responseClass != "proxy") {
			return fmt.Errorf("zstd facts=%+v class=%q", facts.owner, facts.responseClass)
		}
		return nil
	})
}

func BenchmarkAccessWebSocketPath(b *testing.B) {
	for _, withAccess := range []bool{false, true} {
		name := "route-only"
		if withAccess {
			name = "route-plus-access"
		}
		b.Run(name, func(b *testing.B) {
			bridge, err := newAccessBridge(zap.NewNop())
			if err != nil {
				b.Fatal(err)
			}
			registry := newAppRegistry()
			if err := registry.bindAccess(bridge); err != nil {
				b.Fatal(err)
			}
			rec, err := registry.create("hub", []string{"hub.test"}, "")
			if err != nil {
				b.Fatal(err)
			}
			hubs := newHubSet()
			state := &janusState{registry: registry, hubs: hubs, dp: newDataPlane(registry, zap.NewNop())}
			cfg := &hubSite{
				mode: "direct", path: "/hub", maxConns: 1 << 20,
				maxFrame: hubDefaultMaxFrame, maxChannels: hubDefaultMaxChannels, originAny: true,
			}
			app := &App{
				state: state, hubs: hubs, hubLog: zap.NewNop(),
				hubSites: [][]hubSiteEntry{{{patterns: []string{"hub.test"}, cfg: cfg}}},
			}
			handler := &Handler{app: app, dp: state.dp, hubCfg: cfg}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.Host = "hub.test"
				r, facts := accessBenchmarkRequest(r, withAccess)
				var encoder zapcore.Encoder
				if withAccess {
					var err error
					encoder, err = accessBenchmarkEncoder(bridge, r)
					if err != nil {
						b.Errorf("encoder setup: %v", err)
						return
					}
				}
				recorder := caddyhttp.NewResponseRecorder(w, nil, nil)
				if err := handler.ServeHTTP(recorder, r, nil); err != nil {
					b.Errorf("serveHub(): %v", err)
					return
				}
				if withAccess && (facts.owner.state != rec.access || facts.responseClass != "hub" || !facts.upgraded) {
					b.Errorf("hub facts owner=%+v class=%q upgraded=%v",
						facts.owner, facts.responseClass, facts.upgraded)
					return
				}
				if withAccess {
					if err := encodeAccessBenchmarkCompletion(encoder, facts, recorder); err != nil {
						b.Errorf("completion: %v", err)
						return
					}
					if !facts.published.Load() {
						b.Error("access facts were not published")
					}
				}
			}))
			b.Cleanup(server.Close)
			target := "ws" + strings.TrimPrefix(server.URL, "http") + "/hub"
			b.ReportAllocs()
			for b.Loop() {
				client, response, err := websocket.DefaultDialer.Dial(target, nil)
				if err != nil {
					if response != nil {
						_ = response.Body.Close()
					}
					b.Fatal(err)
				}
				_ = client.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				_ = client.Close()
			}
		})
	}
}

func benchmarkAccessPathModes(
	b *testing.B,
	bridge *accessBridge,
	newRequest func() *http.Request,
	serve func(http.ResponseWriter, *http.Request) error,
	verify func(bool, *accessFacts, caddyhttp.ResponseRecorder) error,
) {
	b.Helper()
	for _, withAccess := range []bool{false, true} {
		name := "route-only"
		if withAccess {
			name = "route-plus-access"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				request, facts := accessBenchmarkRequest(newRequest(), withAccess)
				var encoder zapcore.Encoder
				if withAccess {
					var err error
					encoder, err = accessBenchmarkEncoder(bridge, request)
					if err != nil {
						b.Fatal(err)
					}
				}
				destination := httptest.NewRecorder()
				recorder := caddyhttp.NewResponseRecorder(destination, nil, nil)
				if err := serve(recorder, request); err != nil {
					b.Fatal(err)
				}
				if err := verify(withAccess, facts, recorder); err != nil {
					b.Fatal(err)
				}
				if withAccess {
					if err := encodeAccessBenchmarkCompletion(encoder, facts, recorder); err != nil {
						b.Fatal(err)
					}
					if !facts.published.Load() {
						b.Fatal("access facts were not published")
					}
				}
			}
		})
	}
}

func accessBenchmarkRegistry(b *testing.B) (*accessBridge, *appRegistry) {
	b.Helper()
	bridge, err := newAccessBridge(zap.NewNop())
	if err != nil {
		b.Fatal(err)
	}
	registry := newAppRegistry()
	if err := registry.bindAccess(bridge); err != nil {
		b.Fatal(err)
	}
	return bridge, registry
}

func accessBenchmarkRequest(request *http.Request, withAccess bool) (*http.Request, *accessFacts) {
	ctx := context.WithValue(request.Context(), caddyhttp.VarsCtxKey, make(map[string]any))
	if withAccess {
		ctx = context.WithValue(ctx, caddyhttp.ExtraLogFieldsCtxKey, new(caddyhttp.ExtraLogFields))
	}
	request = request.WithContext(ctx)
	caddyhttp.NewTestReplacer(request)
	if !withAccess {
		return request, nil
	}
	return attachAccessFactsRequest(request)
}

func accessBenchmarkEncoder(bridge *accessBridge, request *http.Request) (zapcore.Encoder, error) {
	config := caddylogging.LogEncoderConfig{TimeFormat: "rfc3339_nano", DurationFormat: "seconds"}
	encoder := &AccessEncoder{
		LogEncoderConfig: config,
		wrapped:          zapcore.NewJSONEncoder(config.ZapcoreEncoderConfig()),
		bridge:           bridge,
		logger:           zap.NewNop(),
	}
	if err := encoder.AddObject("request", caddyhttp.LoggableHTTPRequest{Request: request}); err != nil {
		return nil, err
	}
	return encoder, nil
}

func encodeAccessBenchmarkCompletion(
	encoder zapcore.Encoder,
	facts *accessFacts,
	recorder caddyhttp.ResponseRecorder,
) error {
	fields := []zapcore.Field{
		zap.Int("status", recorder.Status()),
		zap.Int("size", recorder.Size()),
		zap.Object("resp_headers", caddyhttp.LoggableHTTPHeader{Header: recorder.Header()}),
		{Key: accessFactsKey, Type: zapcore.SkipType, Interface: facts},
	}
	buf, err := encoder.EncodeEntry(
		zapcore.Entry{Time: time.Now(), Level: zapcore.InfoLevel, Message: "handled request"},
		fields,
	)
	if err != nil {
		return err
	}
	buf.Free()
	return nil
}
