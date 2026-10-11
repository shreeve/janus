package janus

// Measurements for capability 11 (AGENTS.md rule 8; the contract's
// Measurement section): XWB IM HERE round trips through POST /rpc against
// the fake em over real TLS and HTTP/2, on one session and on 64; the
// admission path alone; memory per idle session; and a 64 MiB reply's
// throughput (ReadMemStats stops the world, so the peak is sampled every
// 10 ms). Raw output is kept as docs/*-bench-raw-vista.txt.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

type vbenchEdge struct {
	srv *httptest.Server
	h   *Handler
	url string
}

func newVbenchEdge(b *testing.B) *vbenchEdge {
	b.Helper()
	em := newFakeEm(b)
	h := &Handler{
		app:    &App{state: &janusState{vista: newVistaPool(zap.NewNop())}},
		logger: zap.NewNop(),
		Vista:  []*VistaRoute{{Path: "/rpc", Target: "unix/" + em.Path()}},
	}
	if err := h.provisionVista(); err != nil {
		b.Fatal(err)
	}
	l := h.vistaRoutes[0].limits
	l.maxSessions, l.maxUnsigned, l.perClient, l.perClientUnsigned = 4096, 4096, 4096, 4096
	l.maxReply = 128 << 20
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = h.serveVista(w, r, h.vistaRoutes[0])
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	b.Cleanup(srv.Close)
	return &vbenchEdge{srv: srv, h: h, url: srv.URL + "/rpc"}
}

func (e *vbenchEdge) post(b *testing.B, body []byte, token string) (*http.Response, []byte) {
	req, _ := http.NewRequest(http.MethodPost, e.url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-xwb")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		b.Fatal(err)
	}
	reply, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		b.Fatal(err)
	}
	return resp, reply
}

func (e *vbenchEdge) session(b *testing.B) string {
	resp, _ := e.post(b, xwbTCPConnectMsg(), "")
	for _, c := range resp.Cookies() {
		if c.Name == vistaCookieName {
			return c.Value
		}
	}
	b.Fatalf("TCPConnect answered %d without a token", resp.StatusCode)
	return ""
}

// BenchmarkVistaRoundTrip: one session, one request at a time.
func BenchmarkVistaRoundTrip(b *testing.B) {
	e := newVbenchEdge(b)
	token := e.session(b)
	msg := xwbRPCMsg("XWB IM HERE")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if resp, _ := e.post(b, msg, token); resp.StatusCode != http.StatusOK {
			b.Fatalf("status %d", resp.StatusCode)
		}
	}
}

// BenchmarkVistaRoundTrip64: 64 sessions, each one request at a time.
func BenchmarkVistaRoundTrip64(b *testing.B) {
	e := newVbenchEdge(b)
	tokens := make(chan string, 64)
	for i := 0; i < 64; i++ {
		tokens <- e.session(b)
	}
	msg := xwbRPCMsg("XWB IM HERE")
	b.ReportAllocs()
	b.SetParallelism(64 / runtime.GOMAXPROCS(0))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		token := <-tokens
		defer func() { tokens <- token }()
		for pb.Next() {
			if resp, _ := e.post(b, msg, token); resp.StatusCode != http.StatusOK {
				b.Errorf("status %d", resp.StatusCode)
				return
			}
		}
	})
}

// BenchmarkVistaAdmissionRefusal: the admission path to a refusal that
// never touches em (no token on an RPC: 401), the cost every request pays
// before its stream.
func BenchmarkVistaAdmissionRefusal(b *testing.B) {
	e := newVbenchEdge(b)
	msg := xwbRPCMsg("XWB IM HERE")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if resp, _ := e.post(b, msg, ""); resp.StatusCode != http.StatusUnauthorized {
			b.Fatalf("status %d", resp.StatusCode)
		}
	}
}

// BenchmarkVistaIdleSessionMemory: the heap and goroutines one idle session
// holds in Janus (its reader goroutine, buffers, and maps), over 256.
func BenchmarkVistaIdleSessionMemory(b *testing.B) {
	for i := 0; i < b.N; i++ {
		e := newVbenchEdge(b)
		e.session(b) // warm the client's connection
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		g0 := runtime.NumGoroutine()
		const n = 256
		for j := 0; j < n; j++ {
			e.session(b)
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/n, "heap-B/session")
		b.ReportMetric(float64(runtime.NumGoroutine()-g0)/n, "goroutines/session")
	}
}

// BenchmarkVistaBigReply: a 64 MiB reply streamed through, and the
// process's peak heap while it streams.
func BenchmarkVistaBigReply(b *testing.B) {
	e := newVbenchEdge(b)
	token := e.session(b)
	const size = 64 << 20
	msg := xwbRPCMsg("BIG", strconv.Itoa(size))
	var peak uint64
	var mu sync.Mutex
	stop := make(chan struct{})
	go func() {
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			runtime.ReadMemStats(&ms)
			mu.Lock()
			if ms.HeapInuse > peak {
				peak = ms.HeapInuse
			}
			mu.Unlock()
		}
	}()
	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest(http.MethodPost, e.url, bytes.NewReader(msg))
		req.Header.Set("Content-Type", "application/x-xwb")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := e.srv.Client().Do(req)
		if err != nil {
			b.Fatal(err)
		}
		n, err := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || n < size {
			b.Fatalf("status %d, %d bytes, %v", resp.StatusCode, n, err)
		}
	}
	b.StopTimer()
	close(stop)
	mu.Lock()
	b.ReportMetric(float64(peak)/(1<<20), "peak-heap-MiB")
	mu.Unlock()
}
