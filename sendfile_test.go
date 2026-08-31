package janus

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type closeTracker struct {
	io.Reader
	closed bool
}

func (c *closeTracker) Close() error {
	c.closed = true
	return nil
}

func testSendfileResponse(t *testing.T, method string, status int, name string) (*dataPlane, *http.Response, *closeTracker) {
	t.Helper()
	body := &closeTracker{Reader: strings.NewReader("instruction body must not escape")}
	req := httptest.NewRequest(method, "http://app.test/download", nil)
	resp := &http.Response{
		StatusCode:       status,
		Status:           http.StatusText(status),
		Header:           make(http.Header),
		Body:             body,
		ContentLength:    32,
		TransferEncoding: []string{"chunked"},
		Trailer:          http.Header{"X-Stale": {"trailer"}},
		Request:          req,
	}
	resp.Header.Set(sendfileHeader, name)
	return newDataPlane(newAppRegistry(), nil), resp, body
}

func readSendfileBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSendfileTransformsFinalResponseAndFillsMetadata(t *testing.T) {
	name := filepath.Join(t.TempDir(), "hello.txt")
	if err := os.WriteFile(name, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	dp, resp, upstreamBody := testSendfileResponse(t, http.MethodGet, http.StatusOK, name)
	resp.Header.Set("Content-Length", "999")
	resp.Header.Set("Content-Range", "bytes 0-0/999")
	resp.Header.Set("Trailer", "X-Stale")

	if !dp.applySendfile(resp) {
		t.Fatal("sendfile instruction was not recognized")
	}
	if !upstreamBody.closed {
		t.Fatal("upstream instruction body was not closed")
	}
	if body := readSendfileBody(t, resp); body != "hello world" {
		t.Fatalf("body = %q", body)
	}
	if resp.StatusCode != http.StatusOK || resp.ContentLength != 11 {
		t.Fatalf("status=%d length=%d", resp.StatusCode, resp.ContentLength)
	}
	if resp.Header.Get(sendfileHeader) != "" ||
		resp.Header.Get("Content-Range") != "" ||
		resp.Header.Get("Trailer") != "" ||
		len(resp.TransferEncoding) != 0 ||
		len(resp.Trailer) != 0 {
		t.Fatalf("stale instruction transport metadata survived: %+v", resp)
	}
	for _, header := range []string{"Content-Type", "ETag", "Last-Modified", "Accept-Ranges"} {
		if resp.Header.Get(header) == "" {
			t.Errorf("missing generated %s", header)
		}
	}
	if got := resp.Header.Get("Content-Length"); got != "11" {
		t.Fatalf("Content-Length = %q", got)
	}
}

func TestSendfilePreservesApplicationMetadata(t *testing.T) {
	name := filepath.Join(t.TempDir(), "opaque.bin")
	if err := os.WriteFile(name, []byte("abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	dp, resp, _ := testSendfileResponse(t, http.MethodGet, http.StatusOK, name)
	wantModified := "Mon, 01 Jan 2024 00:00:00 GMT"
	for k, v := range map[string]string{
		"Content-Type":        "application/x-custom",
		"Content-Disposition": `attachment; filename="report.data"`,
		"Content-Encoding":    "br",
		"Cache-Control":       "private, no-store",
		"ETag":                `"application-owned"`,
		"Last-Modified":       wantModified,
		"Accept-Ranges":       "custom",
		"X-Application":       "kept",
	} {
		resp.Header.Set(k, v)
	}
	dp.applySendfile(resp)
	_ = readSendfileBody(t, resp)
	for k, want := range map[string]string{
		"Content-Type":        "application/x-custom",
		"Content-Disposition": `attachment; filename="report.data"`,
		"Content-Encoding":    "br",
		"Cache-Control":       "private, no-store",
		"ETag":                `"application-owned"`,
		"Last-Modified":       wantModified,
		"Accept-Ranges":       "custom",
		"X-Application":       "kept",
		"Content-Length":      "6",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestSendfileHeadConditionsAndRanges(t *testing.T) {
	name := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(name, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("HEAD", func(t *testing.T) {
		dp, resp, _ := testSendfileResponse(t, http.MethodHead, http.StatusOK, name)
		dp.applySendfile(resp)
		if body := readSendfileBody(t, resp); body != "" {
			t.Fatalf("HEAD body = %q", body)
		}
		if resp.StatusCode != 200 || resp.ContentLength != 10 || resp.Header.Get("Content-Length") != "10" {
			t.Fatalf("HEAD status=%d length=%d headers=%v", resp.StatusCode, resp.ContentLength, resp.Header)
		}
	})

	t.Run("not modified", func(t *testing.T) {
		dp, resp, _ := testSendfileResponse(t, http.MethodGet, http.StatusOK, name)
		resp.Header.Set("ETag", `"pin"`)
		resp.Request.Header.Set("If-None-Match", `"pin"`)
		dp.applySendfile(resp)
		if body := readSendfileBody(t, resp); body != "" {
			t.Fatalf("304 body = %q", body)
		}
		if resp.StatusCode != http.StatusNotModified ||
			resp.Header.Get("Content-Type") != "" ||
			resp.Header.Get("Accept-Ranges") != "" {
			t.Fatalf("conditional response: status=%d headers=%v", resp.StatusCode, resp.Header)
		}
	})

	t.Run("precondition failed", func(t *testing.T) {
		dp, resp, _ := testSendfileResponse(t, http.MethodGet, http.StatusOK, name)
		resp.Header.Set("ETag", `"pin"`)
		resp.Request.Header.Set("If-Match", `"other"`)
		dp.applySendfile(resp)
		if body := readSendfileBody(t, resp); body != "" {
			t.Fatalf("412 body = %q", body)
		}
		if resp.StatusCode != http.StatusPreconditionFailed ||
			resp.Header.Get("Content-Type") != "" ||
			resp.Header.Get("Accept-Ranges") != "" {
			t.Fatalf("precondition response: status=%d headers=%v", resp.StatusCode, resp.Header)
		}
	})

	t.Run("single range", func(t *testing.T) {
		dp, resp, _ := testSendfileResponse(t, http.MethodGet, http.StatusOK, name)
		resp.Request.Header.Set("Range", "bytes=2-5")
		dp.applySendfile(resp)
		if body := readSendfileBody(t, resp); body != "2345" {
			t.Fatalf("range body = %q", body)
		}
		if resp.StatusCode != http.StatusPartialContent ||
			resp.Header.Get("Content-Range") != "bytes 2-5/10" ||
			resp.ContentLength != 4 {
			t.Fatalf("range response: status=%d length=%d headers=%v", resp.StatusCode, resp.ContentLength, resp.Header)
		}
	})

	t.Run("multipart range", func(t *testing.T) {
		dp, resp, _ := testSendfileResponse(t, http.MethodGet, http.StatusOK, name)
		resp.Request.Header.Set("Range", "bytes=0-1,8-9")
		dp.applySendfile(resp)
		mediaType := resp.Header.Get("Content-Type")
		boundary := strings.TrimPrefix(mediaType, "multipart/byteranges; boundary=")
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		mr := multipart.NewReader(bytes.NewReader(data), boundary)
		var parts []string
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			value, err := io.ReadAll(part)
			if err != nil {
				t.Fatal(err)
			}
			parts = append(parts, string(value))
		}
		if resp.StatusCode != http.StatusPartialContent || strings.Join(parts, ",") != "01,89" {
			t.Fatalf("multipart status=%d parts=%q", resp.StatusCode, parts)
		}
		if int64(len(data)) != resp.ContentLength ||
			resp.Header.Get("Content-Length") != strconv.Itoa(len(data)) {
			t.Fatalf("multipart bytes=%d response length=%d header=%q",
				len(data), resp.ContentLength, resp.Header.Get("Content-Length"))
		}
	})

	t.Run("unsatisfiable", func(t *testing.T) {
		dp, resp, _ := testSendfileResponse(t, http.MethodGet, http.StatusOK, name)
		resp.Request.Header.Set("Range", "bytes=20-30")
		dp.applySendfile(resp)
		_ = readSendfileBody(t, resp)
		if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable ||
			resp.Header.Get("Content-Range") != "bytes */10" {
			t.Fatalf("range error: status=%d headers=%v", resp.StatusCode, resp.Header)
		}
	})
}

func TestSendfileRejectsInvalidInstructionsWithoutHeaderLeak(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	fifo := filepath.Join(dir, "fifo")
	hasFIFO, err := makeSendfileFIFO(fifo)
	if err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(dir, "valid")
	if err := os.WriteFile(valid, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		path   string
		status int
		mutate func(*http.Response)
	}{
		{name: "empty", path: ""},
		{name: "relative", path: "relative.txt"},
		{name: "NUL", path: "/tmp/a\x00b"},
		{name: "invalid UTF-8", path: string([]byte{'/', 0xff})},
		{name: "missing", path: missing},
		{name: "directory", path: dir},
		{name: "body-forbidden status", path: valid, status: http.StatusNoContent},
		{name: "reset-content status", path: valid, status: http.StatusResetContent},
		{name: "partial-content status", path: valid, status: http.StatusPartialContent},
		{name: "range-error status", path: valid, status: http.StatusRequestedRangeNotSatisfiable},
		{name: "duplicate instruction", path: valid, mutate: func(resp *http.Response) {
			resp.Header[sendfileHeader] = []string{valid, valid}
		}},
		{name: "invalid ETag", path: valid, mutate: func(resp *http.Response) {
			resp.Header.Set("ETag", "not-an-etag")
		}},
		{name: "invalid Last-Modified", path: valid, mutate: func(resp *http.Response) {
			resp.Header.Set("Last-Modified", "yesterday")
		}},
	}
	if hasFIFO {
		tests = append(tests, struct {
			name   string
			path   string
			status int
			mutate func(*http.Response)
		}{name: "FIFO", path: fifo})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := tt.status
			if status == 0 {
				status = http.StatusOK
			}
			dp, resp, upstreamBody := testSendfileResponse(t, http.MethodGet, status, tt.path)
			if tt.mutate != nil {
				tt.mutate(resp)
			}
			if !dp.applySendfile(resp) {
				t.Fatal("invalid instruction was not recognized")
			}
			if !upstreamBody.closed {
				t.Fatal("upstream body was not closed")
			}
			if body := readSendfileBody(t, resp); body != "bad gateway\n" {
				t.Fatalf("body = %q", body)
			}
			if resp.StatusCode != http.StatusBadGateway ||
				resp.Header.Get(sendfileHeader) != "" ||
				resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("invalid response: status=%d headers=%v", resp.StatusCode, resp.Header)
			}
		})
	}
}

func TestSendfilePreservesBodyCapableNon200Status(t *testing.T) {
	name := filepath.Join(t.TempDir(), "error.html")
	if err := os.WriteFile(name, []byte("not found"), 0o644); err != nil {
		t.Fatal(err)
	}
	dp, resp, _ := testSendfileResponse(t, http.MethodGet, http.StatusNotFound, name)
	resp.Request.Header.Set("Range", "bytes=0-2")
	resp.Request.Header.Set("If-None-Match", "*")
	dp.applySendfile(resp)
	if body := readSendfileBody(t, resp); body != "not found" {
		t.Fatalf("body = %q", body)
	}
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Content-Range") != "" {
		t.Fatalf("status=%d headers=%v", resp.StatusCode, resp.Header)
	}
}

type unreadableInstructionBody struct{ closed bool }

func (*unreadableInstructionBody) Read([]byte) (int, error) {
	panic("instruction body must never be read")
}
func (b *unreadableInstructionBody) Close() error {
	b.closed = true
	return nil
}

func TestSendfileNeverReadsInstructionBody(t *testing.T) {
	name := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(name, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	dp, resp, _ := testSendfileResponse(t, http.MethodGet, http.StatusOK, name)
	body := new(unreadableInstructionBody)
	resp.Body = body
	dp.applySendfile(resp)
	if !body.closed {
		t.Fatal("instruction body was not closed")
	}
	_ = readSendfileBody(t, resp)
}

func TestSendfileFIFORejectsWithoutBlocking(t *testing.T) {
	name := filepath.Join(t.TempDir(), "fifo")
	ok, err := makeSendfileFIFO(name)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Skip("platform has no FIFO fixture")
	}
	dp, resp, _ := testSendfileResponse(t, http.MethodGet, http.StatusOK, name)
	done := make(chan struct{})
	go func() {
		dp.applySendfile(resp)
		close(done)
	}()
	select {
	case <-done:
		_ = readSendfileBody(t, resp)
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO instruction blocked during open")
	}
}

func TestSendfileBodyCloseReleasesDescriptor(t *testing.T) {
	name := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(name, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	dp, resp, _ := testSendfileResponse(t, http.MethodGet, http.StatusOK, name)
	dp.applySendfile(resp)
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := resp.Body.Read(make([]byte, 1)); err == nil {
		t.Fatal("sendfile body remained readable after cancellation close")
	}
}

func TestSendfileMultipartCloseReleasesDescriptor(t *testing.T) {
	name := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(name, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	dp, resp, _ := testSendfileResponse(t, http.MethodGet, http.StatusOK, name)
	resp.Request.Header.Set("Range", "bytes=0-1,8-9")
	dp.applySendfile(resp)
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := resp.Body.Read(make([]byte, 1)); err == nil {
		t.Fatal("multipart sendfile body remained readable after cancellation close")
	}
}

func TestSendfileEncodedRangeDescribesFileBytes(t *testing.T) {
	name := filepath.Join(t.TempDir(), "encoded.br")
	if err := os.WriteFile(name, []byte("abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	dp, resp, _ := testSendfileResponse(t, http.MethodGet, http.StatusOK, name)
	resp.Header.Set("Content-Encoding", "br")
	resp.Request.Header.Set("Range", "bytes=1-3")
	dp.applySendfile(resp)
	if body := readSendfileBody(t, resp); body != "bcd" {
		t.Fatalf("body = %q", body)
	}
	if resp.StatusCode != http.StatusPartialContent ||
		resp.ContentLength != 3 ||
		resp.Header.Get("Content-Length") != "3" ||
		resp.Header.Get("Content-Range") != "bytes 1-3/6" ||
		resp.Header.Get("Content-Encoding") != "br" {
		t.Fatalf("encoded range response: status=%d length=%d headers=%v",
			resp.StatusCode, resp.ContentLength, resp.Header)
	}
}

func TestSendfileFailureDoesNotMarkUpstreamUnhealthy(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	missing := filepath.Join(t.TempDir(), "missing")
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(sendfileHeader, missing)
		w.WriteHeader(http.StatusOK)
	}))
	registerApp(t, reg, "app.test", Upstream{Path: sock})

	rr, err := doServe(dp, http.MethodGet, "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusBadGateway || rr.Header().Get(sendfileHeader) != "" {
		t.Fatalf("status=%d headers=%v", rr.Code, rr.Header())
	}
	dp.stateMu.RLock()
	st := dp.state[sock]
	dp.stateMu.RUnlock()
	if st == nil || st.unhealthyNow() {
		t.Fatal("invalid sendfile response marked the upstream unhealthy")
	}
}

func TestClientSendfileRequestHeaderHasNoEffect(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reflected := r.Header.Get(sendfileHeader); reflected != "" {
			w.Header().Set(sendfileHeader, reflected)
		}
		io.WriteString(w, "plain")
	}))
	registerApp(t, reg, "app.test", Upstream{Path: sock})
	req := httptest.NewRequest(http.MethodGet, "http://app.test/", nil)
	req.Header.Set(sendfileHeader, "/etc/passwd")
	rr := httptest.NewRecorder()
	if err := dp.serve(rr, req); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "plain" {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestClientSendfileRequestTrailerHasNoEffect(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if reflected := r.Trailer.Get(sendfileHeader); reflected != "" {
			w.Header().Set(sendfileHeader, reflected)
		}
		io.WriteString(w, "plain")
	}))
	registerApp(t, reg, "app.test", Upstream{Path: sock})
	req := httptest.NewRequest(http.MethodPost, "http://app.test/", strings.NewReader("body"))
	req.Trailer = http.Header{sendfileHeader: {"/etc/passwd"}}
	rr := httptest.NewRecorder()
	if err := dp.serve(rr, req); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "plain" {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestHubBridgeSendfileIsStrippedWithoutFileDelivery(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	name := filepath.Join(t.TempDir(), "must-not-serve")
	if err := os.WriteFile(name, []byte("private file"), 0o644); err != nil {
		t.Fatal(err)
	}
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(sendfileHeader, name)
		io.WriteString(w, "bridge directives")
	}))
	registerApp(t, reg, "app.test", Upstream{Path: sock})
	req := httptest.NewRequest(http.MethodPost, "http://app.test/bridge", nil)
	req = req.WithContext(withRingClass(req.Context(), ringClassBridge))
	rr := httptest.NewRecorder()
	if err := dp.serve(rr, req); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "bridge directives" {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	if rr.Header().Get(sendfileHeader) != "" {
		t.Fatal("bridge response leaked X-Sendfile")
	}
}

func gzipBytes(t *testing.T, value string) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := io.WriteString(zw, value); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSendfileTransportDoesNotMutatePreparedRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://app.test/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req = req.WithContext(context.WithValue(req.Context(), attemptKey{}, &attemptState{autoGzip: true}))
	var upstreamEncoding string
	transport := sendfileTransport{base: roundTripFunc(func(out *http.Request) (*http.Response, error) {
		upstreamEncoding = out.Header.Get("Accept-Encoding")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    out,
		}, nil
	})}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Accept-Encoding") != "gzip" {
		t.Fatalf("input request was mutated: %v", req.Header)
	}
	if upstreamEncoding != "gzip" {
		t.Fatalf("upstream Accept-Encoding = %q", upstreamEncoding)
	}
}

func TestOrdinaryProxyRetainsTransparentGzipBehavior(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	compressed := gzipBytes(t, "ordinary bytes")
	seenEncoding := make(chan string, 1)
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenEncoding <- r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(compressed)))
		_, _ = w.Write(compressed)
	}))
	registerApp(t, reg, "app.test", Upstream{Path: sock})
	rr, err := doServe(dp, http.MethodGet, "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := <-seenEncoding; got != "gzip" {
		t.Fatalf("upstream Accept-Encoding = %q", got)
	}
	if rr.Body.String() != "ordinary bytes" || rr.Header().Get("Content-Encoding") != "" {
		t.Fatalf("body=%q headers=%v", rr.Body.String(), rr.Header())
	}
}

func TestSendfilePreservesGzipRepresentationWithoutClientAcceptEncoding(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	compressed := gzipBytes(t, "encoded file bytes")
	name := filepath.Join(t.TempDir(), "file.gz")
	if err := os.WriteFile(name, compressed, 0o644); err != nil {
		t.Fatal(err)
	}
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(sendfileHeader, name)
		w.Header().Set("Content-Encoding", "gzip")
		io.WriteString(w, "discarded instruction")
	}))
	registerApp(t, reg, "app.test", Upstream{Path: sock})
	rr, err := doServe(dp, http.MethodGet, "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rr.Body.Bytes(), compressed) || rr.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("encoded sendfile bytes=%d/%d headers=%v",
			rr.Body.Len(), len(compressed), rr.Header())
	}
}

type headerCommitRecorder struct {
	header http.Header
	seen   []http.Header
}

func (w *headerCommitRecorder) Header() http.Header { return w.header }
func (w *headerCommitRecorder) WriteHeader(int) {
	w.seen = append(w.seen, w.header.Clone())
}
func (w *headerCommitRecorder) Write(p []byte) (int, error) { return len(p), nil }

func TestSendfileStrippedFromInformationalResponses(t *testing.T) {
	underlying := &headerCommitRecorder{header: make(http.Header)}
	w := &sendfileResponseStripper{ResponseWriter: underlying}
	w.Header().Set(sendfileHeader, "/private/path")
	w.Header().Set("Link", "</style.css>; rel=preload")
	w.WriteHeader(http.StatusEarlyHints)
	if len(underlying.seen) != 1 ||
		underlying.seen[0].Get(sendfileHeader) != "" ||
		underlying.seen[0].Get("Link") == "" {
		t.Fatalf("informational headers = %v", underlying.seen)
	}
}

func TestSendfileStrippedFromResponseTrailers(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", sendfileHeader)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "plain")
		w.Header().Set(sendfileHeader, "/private/path")
	}))
	registerApp(t, reg, "app.test", Upstream{Path: sock})
	rr, err := doServe(dp, http.MethodGet, "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	result := rr.Result()
	if rr.Header().Get(sendfileHeader) != "" || result.Trailer.Get(sendfileHeader) != "" {
		t.Fatalf("X-Sendfile leaked: headers=%v trailers=%v", rr.Header(), result.Trailer)
	}
}

func TestSendfileOnUpgradeReturns502WithoutHealthPenalty(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	name := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(name, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "test")
		w.Header().Set(sendfileHeader, name)
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	registerApp(t, reg, "app.test", Upstream{Path: sock})
	req := httptest.NewRequest(http.MethodGet, "http://app.test/", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "test")
	rr := httptest.NewRecorder()
	if err := dp.serve(rr, req); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusBadGateway || rr.Header().Get(sendfileHeader) != "" {
		t.Fatalf("status=%d headers=%v", rr.Code, rr.Header())
	}
	dp.stateMu.RLock()
	st := dp.state[sock]
	dp.stateMu.RUnlock()
	if st == nil || st.unhealthyNow() {
		t.Fatal("invalid upgrade sendfile marked the upstream unhealthy")
	}
}

type benchmarkResponseWriter struct{ header http.Header }

func (w *benchmarkResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (*benchmarkResponseWriter) WriteHeader(int)             {}
func (*benchmarkResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

func BenchmarkSendfileDelivery(b *testing.B) {
	dir := b.TempDir()
	name := filepath.Join(dir, "large.bin")
	const fileSize = 1 << 20
	if err := os.WriteFile(name, bytes.Repeat([]byte("x"), fileSize), 0o644); err != nil {
		b.Fatal(err)
	}

	socketDir, err := os.MkdirTemp("", "janus-bench")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "u.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		b.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sendfile" {
			w.Header().Set(sendfileHeader, name)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.ServeFile(w, r, name)
	})}
	go server.Serve(listener)
	defer server.Close()

	dp, reg := newTestDataPlane(b)
	registerApp(b, reg, "bench.test", Upstream{Path: socket})
	h := new(Handler)
	rec := AppRecord{Files: &FilesPolicy{
		Roots: []FilesRoot{{Path: dir, Cache: filesCacheRevalidate}},
		Shell: name,
	}}

	for _, bench := range []struct {
		name  string
		serve func(http.ResponseWriter, *http.Request) error
	}{
		{
			name: "registered-file",
			serve: func(w http.ResponseWriter, r *http.Request) error {
				_, err := h.serveFiles(w, r, rec)
				return err
			},
		},
		{name: "proxied-sendfile", serve: dp.serve},
		{name: "proxied-worker-bytes", serve: dp.serve},
	} {
		b.Run(bench.name, func(b *testing.B) {
			target := "/large.bin"
			if bench.name == "proxied-sendfile" {
				target = "/sendfile"
			}
			b.ReportAllocs()
			b.SetBytes(fileSize)
			for b.Loop() {
				req := httptest.NewRequest(http.MethodGet, "http://bench.test"+target, nil)
				out := new(benchmarkResponseWriter)
				if err := bench.serve(out, req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
