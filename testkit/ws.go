package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// dialHub connects to the local edge (127.0.0.1:443) with SNI host and
// completes the /hub WebSocket handshake. Verification off: the driver is
// a fixture instrument and TLS trust is proven by the curl-based cases
// (no curl -k anywhere in the suite).
func dialHub(host, origin, cookie string, timeout time.Duration) (*websocket.Conn, *http.Response, error) {
	d := websocket.Dialer{
		NetDial: func(network, addr string) (net.Conn, error) {
			return net.DialTimeout("tcp", "127.0.0.1:443", timeout)
		},
		TLSClientConfig:  &tls.Config{ServerName: host, InsecureSkipVerify: true},
		HandshakeTimeout: timeout,
	}
	hdr := http.Header{}
	if origin != "-" && origin != "" {
		hdr.Set("Origin", origin)
	}
	if cookie != "-" && cookie != "" {
		hdr.Set("Cookie", cookie)
	}
	return d.Dial("wss://"+host+"/hub", hdr)
}

// cmdWedge: complete the handshake, then never read — the slow-consumer
// instrument. The kernel buffers fill and the edge's outbound queue trips.
func cmdWedge(args []string) {
	var host string
	flagValues(args, map[string]*string{"--host": &host})
	if host == "" {
		die("usage: testkit wedge --host H")
	}
	conn, resp, err := dialHub(host, "-", "-", 5*time.Second)
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		die("testkit wedge: never opened (%s): %v", status, err)
	}
	// A bare select{} would trip the runtime's deadlock detector and kill
	// the process (un-wedging the instrument); sleep instead, keeping
	// conn referenced so nothing can close the fd underneath us.
	time.Sleep(time.Hour) // never read again
	conn.Close()
}

// cmdWS: the hub acceptance driver — an RFC 6455 client with a command
// DSL. Usage: testkit ws <host> <origin|-> <cookie|-> <cmd…>
// Commands:
//
//	send=<json>          send a text frame
//	sendbig=<bytes>      send {"blob":"xx…"} of roughly that many bytes
//	binary               send a binary frame
//	expect=<substr>      wait ≤5s for a frame containing substr; print RECV
//	noframe=<ms>         assert no frame arrives within ms
//	expectclose=<code>[,<substr>]  wait ≤10s for close; print CLOSE
//	id                   loopback probe; print "ID <connection id>"
//	touch=<file>         write a flag file (test-side coordination)
//	waitfile=<file>      wait ≤30s for a flag file
//	pause=<ms>           sleep
//	close                client-initiated close 1000
func cmdWS(args []string) {
	if len(args) < 3 {
		die("usage: testkit ws HOST ORIGIN|- COOKIE|- CMD…")
	}
	host, origin, cookie := args[0], args[1], args[2]
	cmds := args[3:]

	fail := func(msg string) {
		fmt.Println("FAIL " + msg)
		os.Exit(1)
	}

	conn, resp, err := dialHub(host, origin, cookie, 10*time.Second)
	if err != nil {
		if resp != nil {
			fail(fmt.Sprintf("never opened: HTTP/1.1 %s", resp.Status))
		}
		fail("never opened: " + err.Error())
	}

	type closeInfo struct {
		code   int
		reason string
	}
	var (
		mu     sync.Mutex
		queue  []string
		closed *closeInfo
	)
	// The default close handler echoes the peer's close; the default ping
	// handler answers pong — both match the driver contract.
	go func() {
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				mu.Lock()
				if ce, ok := err.(*websocket.CloseError); ok {
					closed = &closeInfo{ce.Code, ce.Text}
				}
				mu.Unlock()
				return
			}
			if mt == websocket.TextMessage {
				mu.Lock()
				queue = append(queue, string(data))
				mu.Unlock()
			}
		}
	}()

	send := func(mt int, payload []byte) {
		if err := conn.WriteMessage(mt, payload); err != nil {
			fail("send: " + err.Error())
		}
	}
	// waitFrame scans (and pops from) the queue until deadline; a received
	// close ends the wait early, exactly like a dead socket would.
	waitFrame := func(sub string, timeout time.Duration) (string, bool) {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			mu.Lock()
			for i, m := range queue {
				if strings.Contains(m, sub) {
					queue = append(queue[:i], queue[i+1:]...)
					mu.Unlock()
					return m, true
				}
			}
			done := closed != nil
			mu.Unlock()
			if done {
				return "", false
			}
			time.Sleep(10 * time.Millisecond)
		}
		return "", false
	}

	for _, cmd := range cmds {
		switch {
		case strings.HasPrefix(cmd, "send="):
			send(websocket.TextMessage, []byte(cmd[5:]))
		case strings.HasPrefix(cmd, "sendbig="):
			n, err := strconv.Atoi(cmd[8:])
			if err != nil {
				fail("bad sendbig " + cmd)
			}
			send(websocket.TextMessage, []byte(`{"blob":"`+strings.Repeat("x", n)+`"}`))
		case cmd == "binary":
			send(websocket.BinaryMessage, []byte{0x01, 0x02, 0x03})
		case strings.HasPrefix(cmd, "expect="):
			sub := cmd[7:]
			m, ok := waitFrame(sub, 5*time.Second)
			if !ok {
				mu.Lock()
				fail(fmt.Sprintf("no frame with %s (closed=%v queue=%v)", sub, closed, queue))
			}
			fmt.Println("RECV " + m)
		case strings.HasPrefix(cmd, "noframe="):
			ms, err := strconv.Atoi(cmd[8:])
			if err != nil {
				fail("bad noframe " + cmd)
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)
			mu.Lock()
			if len(queue) > 0 {
				fail("unexpected frame " + queue[0])
			}
			mu.Unlock()
		case strings.HasPrefix(cmd, "expectclose="):
			want := strings.SplitN(cmd[12:], ",", 2)
			deadline := time.Now().Add(10 * time.Second)
			for {
				mu.Lock()
				c := closed
				mu.Unlock()
				if c != nil {
					if strconv.Itoa(c.code) != want[0] {
						fail(fmt.Sprintf("close %d %q (want %s)", c.code, c.reason, want[0]))
					}
					if len(want) > 1 && !strings.Contains(c.reason, want[1]) {
						fail(fmt.Sprintf("close reason %q (want %s)", c.reason, want[1]))
					}
					fmt.Printf("CLOSE %d %s\n", c.code, c.reason)
					break
				}
				if !time.Now().Before(deadline) {
					fail("never closed")
				}
				time.Sleep(10 * time.Millisecond)
			}
		case cmd == "id":
			send(websocket.TextMessage, []byte(`{"__whoami!":1}`))
			m, ok := waitFrame("__whoami", 5*time.Second)
			if !ok {
				fail("no whoami loopback")
			}
			var frame map[string]any
			if err := json.Unmarshal([]byte(m), &frame); err != nil {
				fail("bad whoami frame " + m)
			}
			from, _ := frame["<"].([]any)
			if len(from) == 0 {
				fail("whoami frame lacks provenance: " + m)
			}
			id, _ := from[0].(string)
			fmt.Println("ID " + id)
		case strings.HasPrefix(cmd, "touch="):
			if err := os.WriteFile(cmd[6:], []byte("1"), 0o644); err != nil {
				fail("touch: " + err.Error())
			}
		case strings.HasPrefix(cmd, "waitfile="):
			f := cmd[9:]
			t0 := time.Now()
			for {
				if _, err := os.Stat(f); err == nil {
					break
				}
				if time.Since(t0) > 30*time.Second {
					fail("waitfile " + f)
				}
				time.Sleep(50 * time.Millisecond)
			}
		case strings.HasPrefix(cmd, "pause="):
			ms, err := strconv.Atoi(cmd[6:])
			if err != nil {
				fail("bad pause " + cmd)
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)
		case cmd == "close":
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"),
				time.Now().Add(5*time.Second))
		default:
			fail("unknown cmd " + cmd)
		}
	}
	fmt.Println("DONE")
}
