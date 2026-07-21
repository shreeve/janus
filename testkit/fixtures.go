package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const controlBase = "http://127.0.0.1:7600"

// serveUnix removes any stale socket, listens, and serves forever.
func serveUnix(sock string, handler http.Handler) {
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		die("testkit: listen %s: %v", sock, err)
	}
	die("testkit: serve %s: %v", sock, http.Serve(l, handler))
}

// appendLine appends s + "\n" to path (O_APPEND: concurrent writers safe).
func appendLine(path, s string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, s)
}

func flagValues(args []string, spec map[string]*string) []string {
	var rest []string
	for i := 0; i < len(args); i++ {
		if p, ok := spec[args[i]]; ok && i+1 < len(args) {
			*p = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return rest
}

// sendText answers with an explicit Content-Length (never chunked — the
// cache group's max_body case depends on a declared 300KiB body).
func sendText(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	io.WriteString(w, body)
}

// cmdUpstream: HTTP/1.1 echo server on a unix socket.
// GET → "upstream:NAME"; POST → append body to the hitfile, echo it back.
func cmdUpstream(args []string) {
	var sock, name, hits string
	flagValues(args, map[string]*string{"--sock": &sock, "--name": &name, "--hits": &hits})
	if sock == "" || name == "" || hits == "" {
		die("usage: testkit upstream --sock S --name N --hits F")
	}
	serveUnix(sock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			sendText(w, 200, "upstream:"+name+"\n")
		case "POST":
			body, _ := io.ReadAll(r.Body)
			appendLine(hits, string(body))
			sendText(w, 200, "received:"+string(body)+"\n")
		default:
			sendText(w, 501, "")
		}
	}))
}

// cmdDoorbell: on GET /ring, PUT the new socket as the app's real upstream
// via /1.0 (awaits the 200), then answer 204.
func cmdDoorbell(args []string) {
	var sock, app, newsock, ring string
	flagValues(args, map[string]*string{
		"--sock": &sock, "--app": &app, "--newsock": &newsock, "--ring": &ring})
	if sock == "" || app == "" || newsock == "" || ring == "" {
		die("usage: testkit doorbell --sock S --app ID --newsock S2 --ring F")
	}
	serveUnix(sock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/ring" {
			w.WriteHeader(404)
			return
		}
		appendLine(ring, "ring")
		body, _ := json.Marshal(map[string]any{
			"upstreams": []map[string]string{{"path": newsock}},
		})
		req, _ := http.NewRequest("PUT",
			controlBase+"/1.0/apps/"+app+"/upstreams", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			die("testkit doorbell: PUT upstreams: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			die("testkit doorbell: PUT upstreams -> %d", resp.StatusCode)
		}
		w.WriteHeader(204)
	}))
}

// cmdCacheUpstream: the cache group's scripted origin — per-path sleeps,
// cookies, cache-control vetoes, Vary variants, gzip, a truncated body
// with a real mid-stream FIN, and an oversized body.
func cmdCacheUpstream(args []string) {
	var sock, hits string
	flagValues(args, map[string]*string{"--sock": &sock, "--hits": &hits})
	if sock == "" || hits == "" {
		die("usage: testkit cacheup --sock S --hits F")
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte("compressed-body\n"))
	zw.Close()

	send := func(w http.ResponseWriter, code int, body string, headers map[string]string) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		sendText(w, code, body)
	}
	serveUnix(sock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appendLine(hits, r.URL.RequestURI())
		if r.Method == "POST" {
			io.Copy(io.Discard, r.Body)
			send(w, 200, "posted\n", nil)
			return
		}
		switch r.URL.Path {
		case "/slow":
			time.Sleep(500 * time.Millisecond)
			send(w, 200, "slow-body\n", nil)
		case "/slower":
			time.Sleep(1500 * time.Millisecond)
			send(w, 200, "slower-body\n", nil)
		case "/slowcookie":
			time.Sleep(500 * time.Millisecond)
			send(w, 200, "personal\n", map[string]string{"Set-Cookie": "sid=1"})
		case "/setcookie":
			send(w, 200, "cookie\n", map[string]string{"Set-Cookie": "sid=1"})
		case "/nostore":
			send(w, 200, "nostore\n", map[string]string{"Cache-Control": "no-store"})
		case "/private":
			send(w, 200, "private\n", map[string]string{"Cache-Control": "private"})
		case "/badcc":
			send(w, 200, "badcc\n", map[string]string{"Cache-Control": "max-age=="})
		case "/expires":
			send(w, 200, "expires\n", map[string]string{"Expires": "Fri, 01 Jan 2100 00:00:00 GMT"})
		case "/ce":
			send(w, 200, gz.String(), map[string]string{"Content-Encoding": "gzip"})
		case "/cevary":
			send(w, 200, gz.String(), map[string]string{
				"Content-Encoding": "gzip", "Vary": "Accept-Encoding"})
		case "/acao-echo":
			origin := r.Header.Get("Origin")
			if origin == "" {
				origin = "none"
			}
			send(w, 200, "acao\n", map[string]string{"Access-Control-Allow-Origin": origin})
		case "/acao-star":
			send(w, 200, "acao\n", map[string]string{"Access-Control-Allow-Origin": "*"})
		case "/vary-lang":
			lang := r.Header.Get("Accept-Language")
			if lang == "" {
				lang = "none"
			}
			send(w, 200, "lang:"+lang+"\n", map[string]string{"Vary": "Accept-Language"})
		case "/vary-star":
			send(w, 200, "varystar\n", map[string]string{"Vary": "*"})
		case "/404":
			send(w, 404, "nope\n", nil)
		case "/500":
			send(w, 500, "boom\n", nil)
		case "/busy":
			send(w, 503, "busy\n", map[string]string{"Rip-Worker-Busy": "1", "Retry-After": "0"})
		case "/truncate":
			// Declare 1000 bytes, deliver 500, then a real FIN mid-body:
			// hijack for full control of the connection lifetime.
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 1000\r\n\r\n")
			buf.WriteString(strings.Repeat("x", 500))
			buf.Flush()
			conn.Close()
		case "/big":
			send(w, 200, strings.Repeat("B", 300*1024), nil)
		default:
			send(w, 200, "plain:"+r.URL.RequestURI()+"\n", nil)
		}
	}))
}

// pyStr JSON-encodes a string the way the bridge log's consumers expect:
// no HTML escaping, quotes and backslashes escaped.
func pyStr(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimRight(b.String(), "\n")
}

// pyVal renders a string-or-absent header value: missing → null.
func pyVal(s string, ok bool) string {
	if !ok {
		return "null"
	}
	return pyStr(s)
}

// cmdHubTenant: the recording, scriptable bridge tenant. Serves POST
// {bridge_path} (records as JSONL + answers from the playbook file),
// answers any other request with plain:<path>, and heartbeats every app
// id given (500ms; the suite's TTL is 2s).
func cmdHubTenant(args []string) {
	var sock, hits, playbook string
	appIDs := flagValues(args, map[string]*string{
		"--sock": &sock, "--hits": &hits, "--playbook": &playbook})
	if sock == "" || hits == "" || playbook == "" {
		die("usage: testkit hubtenant --sock S --hits F --playbook F ID…")
	}

	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		for {
			for _, app := range appIDs {
				req, _ := http.NewRequest("POST",
					controlBase+"/1.0/apps/"+app+"/heartbeat", nil)
				if resp, err := client.Do(req); err == nil {
					resp.Body.Close() // deleted/reaped apps are part of the tests
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()

	var mu sync.Mutex
	serveUnix(sock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			sendText(w, 200, "plain:"+r.URL.RequestURI()+"\n")
			return
		}
		kind := r.Header.Get("Sec-WebSocket-Frame")
		body, _ := io.ReadAll(r.Body)
		if kind == "" {
			sendText(w, 200, "plain-post\n")
			return
		}
		// The record's key spelling and ": " separators are load-bearing:
		// test.sh greps the JSONL verbatim.
		_, hasKey := r.Header["Sec-Websocket-Key"]
		_, hasConn := r.Header["Connection"]
		rec := fmt.Sprintf(`{"kind": %s, "path": %s, "client": %s, "app": %s, `+
			`"cookie": %s, "has_sec_ws_key": %t, "has_connection": %t, `+
			`"content_type": %s, "body": %s}`,
			pyStr(kind),
			pyStr(r.URL.RequestURI()),
			pyVal(r.Header.Get("Janus-Hub-Client"), r.Header.Get("Janus-Hub-Client") != ""),
			pyVal(r.Header.Get("Janus-Hub-App"), r.Header.Get("Janus-Hub-App") != ""),
			pyVal(r.Header.Get("Cookie"), r.Header.Get("Cookie") != ""),
			hasKey, hasConn,
			pyVal(r.Header.Get("Content-Type"), r.Header.Get("Content-Type") != ""),
			pyStr(string(body)))
		mu.Lock()
		appendLine(hits, rec)
		mu.Unlock()

		var play map[string]struct {
			Status  int    `json:"status"`
			Body    string `json:"body"`
			DelayMS int    `json:"delay_ms"`
		}
		if data, err := os.ReadFile(playbook); err == nil {
			_ = json.Unmarshal(data, &play)
		}
		act := play[kind]
		if act.DelayMS > 0 {
			time.Sleep(time.Duration(act.DelayMS) * time.Millisecond)
		}
		status := act.Status
		if status == 0 {
			status = 204
		}
		w.Header().Set("Content-Type", "application/json")
		if act.Body != "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(act.Body)))
		}
		w.WriteHeader(status)
		if act.Body != "" {
			io.WriteString(w, act.Body)
		}
	}))
}
