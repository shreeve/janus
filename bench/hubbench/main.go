// Command hubbench is the hub arm of the bench harness: the load client,
// publisher, and fixture tenant for the six Phase 7 measurements
// (docs/20260720-162350-hub-design.md "Bench plan"). It measures, never
// gates — nothing here is part of the test contract.
//
// Modes (-mode):
//
//	tenant  the bridge fixture: a unix-socket HTTP server answering every
//	        bridge POST 204 (open fast always; text after -text-delay),
//	        heartbeating its app ids every 5s
//	subs    N concurrent wss subscribers on one channel; counts delivered
//	        bundles, computes publish→receive latency percentiles from the
//	        publisher's embedded timestamps, tracks the largest
//	        inter-delivery gap (stall detector) and close codes
//	pub     paced publisher: POST /1.0/apps/{id}/hub/publish at -rate/s
//	        (0 = unpaced max) with a nanosecond timestamp in every payload
//	send    N wss clients each sending no-delivery text frames as fast as
//	        the socket accepts (bare event, no "@": executes at the edge,
//	        forwards to the text bridge, delivers to nobody) — the
//	        text-bridge-tax instrument
//	ramp    open N connections as fast as -dial-conc allows, report
//	        admitted conns/s, then hold them idle for -hold (the
//	        connection-ceiling / idle-RSS instrument)
//
// TLS: the client dials 127.0.0.1:443 with InsecureSkipVerify — acceptable
// for the bench client ONLY; real trust in the committed *.ripdev.io chain
// is proven by the acceptance suite (test.sh, no curl -k anywhere).
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var (
	mode      = flag.String("mode", "", "tenant | subs | pub | send | ramp")
	host      = flag.String("host", "hubany.ripdev.io", "hub site (SNI + Host); dialed at 127.0.0.1:443")
	channel   = flag.String("channel", "/bench", "hub channel")
	n         = flag.Int("n", 1, "connections (subs/send/ramp)")
	dur       = flag.Duration("dur", 15*time.Second, "measurement window (starts after READY for subs)")
	dialConc  = flag.Int("dial-conc", 64, "parallel dialers")
	wedge     = flag.Int("wedge", 0, "subs that never read after subscribing (slow-consumer instrument)")
	rate      = flag.Int("rate", 0, "pub: publishes/s (0 = unpaced max)")
	conc      = flag.Int("conc", 4, "pub: concurrent HTTP publishers")
	pad       = flag.Int("pad", 0, "pub: filler bytes per payload")
	app       = flag.String("app", "", "app id (pub/tenant)")
	control   = flag.String("control", "http://127.0.0.1:7600", "control plane base")
	sock      = flag.String("sock", "", "tenant: unix socket path")
	textDelay = flag.Duration("text-delay", 0, "tenant: delay before answering text bridges")
	hold      = flag.Duration("hold", 60*time.Second, "ramp: idle hold after all conns are up")
)

func main() {
	flag.Parse()
	switch *mode {
	case "tenant":
		runTenant()
	case "subs":
		runSubs()
	case "pub":
		runPub()
	case "send":
		runSend()
	case "ramp":
		runRamp()
	default:
		fmt.Fprintln(os.Stderr, "hubbench: -mode must be tenant | subs | pub | send | ramp")
		os.Exit(2)
	}
}

func dialer() *websocket.Dialer {
	return &websocket.Dialer{
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", "127.0.0.1:443")
		},
		// Bench client only — see the package comment.
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: true, ServerName: *host},
		HandshakeTimeout: 20 * time.Second,
	}
}

func dialHub() (*websocket.Conn, error) {
	c, resp, err := dialer().Dial("wss://"+*host+"/hub", nil)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("%w (status %d)", err, resp.StatusCode)
		}
		return nil, err
	}
	return c, nil
}

// dialAll opens count connections with dial-conc parallel workers, running
// setup on each. It returns the live conns and the wall time taken.
func dialAll(count int, setup func(*websocket.Conn) error) ([]*websocket.Conn, time.Duration, int) {
	conns := make([]*websocket.Conn, count)
	var idx, fails atomic.Int64
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *dialConc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(idx.Add(1)) - 1
				if i >= count {
					return
				}
				// Retry across transient 503 windows: a mass disconnect from
				// the previous leg can flood the fixture's close bridge and
				// trip passive health for its 2s window — recovery is by
				// design, so the dialer rides it out instead of losing a leg.
				var c *websocket.Conn
				var err error
				for attempt := 0; attempt < 8; attempt++ {
					c, err = dialHub()
					if err == nil {
						break
					}
					time.Sleep(time.Second)
				}
				if err == nil && setup != nil {
					err = setup(c)
				}
				if err != nil {
					fails.Add(1)
					fmt.Fprintf(os.Stderr, "dial %d: %v\n", i, err)
					continue
				}
				conns[i] = c
			}
		}()
	}
	wg.Wait()
	live := conns[:0]
	for _, c := range conns {
		if c != nil {
			live = append(live, c)
		}
	}
	return live, time.Since(start), int(fails.Load())
}

func subscribe(c *websocket.Conn) error {
	return c.WriteMessage(websocket.TextMessage, []byte(`{"+":["`+*channel+`"]}`))
}

// extractT pulls the publisher's `"t":<nanos>` out of a delivered payload
// without a JSON parse (the read loop is the hot path of the instrument).
func extractT(p []byte) (int64, bool) {
	i := strings.Index(string(p), `"t":`)
	if i < 0 {
		return 0, false
	}
	var v int64
	seen := false
	for _, b := range p[i+4:] {
		if b < '0' || b > '9' {
			break
		}
		v = v*10 + int64(b-'0')
		seen = true
	}
	return v, seen
}

func atomicMax(a *atomic.Int64, v int64) {
	for {
		cur := a.Load()
		if v <= cur || a.CompareAndSwap(cur, v) {
			return
		}
	}
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}

// --- subs -------------------------------------------------------------------

func runSubs() {
	conns, dialTime, fails := dialAll(*n, subscribe)
	nWedged := *wedge
	if nWedged > len(conns) {
		nWedged = len(conns)
	}
	fmt.Printf("READY n=%d fails=%d wedged=%d dial_ms=%d\n",
		len(conns), fails, nWedged, dialTime.Milliseconds())

	deadline := time.Now().Add(*dur)
	var (
		recv, unexpected atomic.Int64
		firstNs, lastNs  atomic.Int64
		maxGapNs         atomic.Int64
		closeMu          sync.Mutex
		closes           = map[int]int{}
		latMu            sync.Mutex
		lats             []float64
	)
	var wg sync.WaitGroup
	for i, c := range conns {
		if i < nWedged {
			continue // wedged: subscribed, never reads — the queue caps close it
		}
		wg.Add(1)
		go func(c *websocket.Conn) {
			defer wg.Done()
			var local []float64
			lastRecv := int64(0)
			nRecv := 0
			c.SetReadDeadline(deadline)
			for {
				_, p, err := c.ReadMessage()
				now := time.Now().UnixNano()
				if err != nil {
					if time.Now().After(deadline) {
						break // window over; any error here is shutdown noise
					}
					if ce, ok := err.(*websocket.CloseError); ok {
						closeMu.Lock()
						closes[ce.Code]++
						closeMu.Unlock()
					}
					unexpected.Add(1)
					break
				}
				recv.Add(1)
				nRecv++
				firstNs.CompareAndSwap(0, now)
				atomicMax(&lastNs, now)
				if lastRecv != 0 {
					atomicMax(&maxGapNs, now-lastRecv)
				}
				lastRecv = now
				// Sample every delivery up to 10k per conn, then every 16th.
				if t, ok := extractT(p); ok && (nRecv <= 10000 || nRecv%16 == 0) {
					local = append(local, float64(now-t)/1e6)
				}
			}
			latMu.Lock()
			lats = append(lats, local...)
			latMu.Unlock()
		}(c)
	}
	wg.Wait()
	for _, c := range conns {
		c.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"),
			time.Now().Add(time.Second))
		c.Close()
	}
	sort.Float64s(lats)
	window := float64(lastNs.Load()-firstNs.Load()) / 1e9
	rate := 0.0
	if window > 0 {
		rate = float64(recv.Load()) / window
	}
	closeMu.Lock()
	cl, _ := json.Marshal(closes)
	closeMu.Unlock()
	fmt.Printf("SUBS n=%d recv=%d window_s=%.2f rate=%.0f/s p50=%.2fms p99=%.2fms max=%.2fms maxgap_ms=%.0f unexpected_closes=%d closes=%s samples=%d\n",
		len(conns), recv.Load(), window, rate,
		pct(lats, 0.50), pct(lats, 0.99), pct(lats, 1.0),
		float64(maxGapNs.Load())/1e6, unexpected.Load(), cl, len(lats))
}

// --- pub --------------------------------------------------------------------

func runPub() {
	if *app == "" {
		fmt.Fprintln(os.Stderr, "pub: -app is required")
		os.Exit(2)
	}
	url := *control + "/1.0/apps/" + *app + "/hub/publish"
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: *conc}}
	filler := strings.Repeat("x", *pad)

	deadline := time.Now().Add(*dur)
	tokens := make(chan struct{}, *conc)
	go func() {
		defer close(tokens)
		if *rate <= 0 { // unpaced: workers spin until the deadline
			for time.Now().Before(deadline) {
				tokens <- struct{}{}
			}
			return
		}
		start := time.Now()
		for i := 0; ; i++ {
			next := start.Add(time.Duration(float64(i) * float64(time.Second) / float64(*rate)))
			if next.After(deadline) {
				return
			}
			time.Sleep(time.Until(next))
			tokens <- struct{}{}
		}
	}()

	var published, enqueued, errs atomic.Int64
	var seq atomic.Int64
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range tokens {
				body := fmt.Sprintf(`{"@":[%q],"tick":{"t":%d,"seq":%d,"pad":%q}}`,
					*channel, time.Now().UnixNano(), seq.Add(1), filler)
				resp, err := client.Post(url, "application/json", strings.NewReader(body))
				if err != nil {
					errs.Add(1)
					continue
				}
				var out struct {
					Deliveries int64 `json:"deliveries"`
				}
				json.NewDecoder(resp.Body).Decode(&out)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					errs.Add(1)
					continue
				}
				published.Add(1)
				enqueued.Add(out.Deliveries)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	fmt.Printf("PUB rate_target=%d published=%d achieved=%.0f/s enq_deliveries=%d enq_rate=%.0f/s errors=%d dur_s=%.1f\n",
		*rate, published.Load(), float64(published.Load())/elapsed,
		enqueued.Load(), float64(enqueued.Load())/elapsed, errs.Load(), elapsed)
}

// --- send -------------------------------------------------------------------

func runSend() {
	conns, dialTime, fails := dialAll(*n, nil)
	fmt.Printf("READY n=%d fails=%d dial_ms=%d\n", len(conns), fails, dialTime.Milliseconds())

	deadline := time.Now().Add(*dur)
	var frames, errs atomic.Int64
	var maxElapsed atomic.Int64
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func(i int, c *websocket.Conn) {
			defer wg.Done()
			pong := make(chan struct{})
			marker := fmt.Sprintf("sync-%d", i)
			go func() { // reader: protocol pings answered inside ReadMessage; watch for our pong
				for {
					_, p, err := c.ReadMessage()
					if err != nil {
						return
					}
					if strings.Contains(string(p), marker) {
						close(pong)
						return
					}
				}
			}()
			// Bare event, no "@": validates + bridges at the edge, delivers
			// to nobody (sender-excluded default target) — pure send cost.
			frame := []byte(`{"note":{"t":1}}`)
			start := time.Now()
			sent := int64(0)
			for time.Now().Before(deadline) {
				if err := c.WriteMessage(websocket.TextMessage, frame); err != nil {
					errs.Add(1)
					break
				}
				sent++
			}
			// The edge answers "?" after the frames ahead of it execute:
			// the pong bounds the whole burst's edge-execution time.
			c.WriteMessage(websocket.TextMessage, []byte(`{"?":"`+marker+`"}`))
			select {
			case <-pong:
			case <-time.After(30 * time.Second):
				errs.Add(1)
			}
			frames.Add(sent)
			atomicMax(&maxElapsed, int64(time.Since(start)))
			c.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"),
				time.Now().Add(time.Second))
			c.Close()
		}(i, c)
	}
	wg.Wait()
	elapsed := time.Duration(maxElapsed.Load()).Seconds()
	rate := 0.0
	if elapsed > 0 {
		rate = float64(frames.Load()) / elapsed
	}
	fmt.Printf("SEND n=%d frames=%d elapsed_s=%.2f rate=%.0f/s errors=%d\n",
		len(conns), frames.Load(), elapsed, rate, errs.Load())
}

// --- ramp -------------------------------------------------------------------

func runRamp() {
	conns, dialTime, fails := dialAll(*n, subscribe)
	rate := float64(len(conns)) / dialTime.Seconds()
	fmt.Printf("RAMP n=%d fails=%d elapsed_s=%.2f rate=%.0f conns/s\n",
		len(conns), fails, dialTime.Seconds(), rate)
	fmt.Printf("HOLDING %v\n", *hold)
	deadline := time.Now().Add(*hold)
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c *websocket.Conn) { // idle reader: keeps protocol pings answered
			defer wg.Done()
			c.SetReadDeadline(deadline)
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		}(c)
	}
	wg.Wait()
	for _, c := range conns {
		c.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"),
			time.Now().Add(time.Second))
		c.Close()
	}
	fmt.Println("RAMP DONE")
}

// --- tenant -----------------------------------------------------------------

func runTenant() {
	if *sock == "" {
		fmt.Fprintln(os.Stderr, "tenant: -sock is required")
		os.Exit(2)
	}
	os.Remove(*sock)
	ln, err := net.Listen("unix", *sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tenant:", err)
		os.Exit(1)
	}
	apps := strings.Split(*app, ",")
	go func() { // heartbeat every 5s (registry TTL is 15s)
		client := &http.Client{Timeout: 2 * time.Second}
		for {
			for _, id := range apps {
				if id == "" {
					continue
				}
				req, _ := http.NewRequest("POST", *control+"/1.0/apps/"+id+"/heartbeat", nil)
				if resp, err := client.Do(req); err == nil {
					resp.Body.Close()
				}
			}
			time.Sleep(5 * time.Second)
		}
	}()
	var opens, texts, closes atomic.Int64
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Sec-WebSocket-Frame") {
		case "open":
			opens.Add(1)
		case "text":
			texts.Add(1)
			if *textDelay > 0 {
				time.Sleep(*textDelay)
			}
		case "close":
			closes.Add(1)
		default:
			w.Write([]byte("ok\n"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	fmt.Printf("TENANT sock=%s apps=%s text_delay=%v\n", *sock, *app, *textDelay)
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "tenant:", err)
		os.Exit(1)
	}
}
