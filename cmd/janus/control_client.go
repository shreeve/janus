package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// edgeControl owns one endpoint and its pooled transport for a command's
// lifetime. Mutations never retry against a different listener after an
// ambiguous failure: the first request might already have committed.
type edgeControl struct {
	client        *http.Client
	base, address string
}

func controlClient(dial func(context.Context, string, string) (net.Conn, error)) *http.Client {
	tr := &http.Transport{IdleConnTimeout: 30 * time.Second, MaxIdleConns: 2, MaxIdleConnsPerHost: 2}
	if dial != nil {
		tr.DialContext = dial
	}
	return &http.Client{Transport: tr, Timeout: 1500 * time.Millisecond}
}

func (c *edgeControl) Close() { c.client.CloseIdleConnections() }

func (c *edgeControl) Do(method, path string, body []byte) ([]byte, int, error) {
	r, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(r)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

func (c *edgeControl) Get(path string) ([]byte, error) {
	b, status, err := c.Do(http.MethodGet, path, nil)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("%s: HTTP %d", path, status)
	}
	return b, err
}

func openEdgeControl(p servicePaths) (*edgeControl, []byte, error) {
	if socketExists(p.sock) {
		c := &edgeControl{client: controlClient(func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", p.sock)
		}), base: "http://janus", address: "unix " + p.sock}
		if b, err := c.Get("/1.0"); err == nil && controlIdentifies(b, p.sock) {
			return c, b, nil
		}
		c.Close()
	}
	c := &edgeControl{client: controlClient(nil), base: localControlURL, address: localControlURL}
	b, err := c.Get("/1.0")
	if err == nil && controlIdentifies(b, p.sock) {
		return c, b, nil
	}
	c.Close()
	return nil, nil, errors.New("no control plane answered for this edge")
}

func controlIdentifies(b []byte, sock string) bool {
	var root struct {
		Type    string `json:"type"`
		Control []struct {
			Mode   string `json:"mode"`
			Listen string `json:"listen"`
		} `json:"control"`
	}
	if json.Unmarshal(b, &root) != nil || root.Type != "janus" {
		return false
	}
	for _, c := range root.Control {
		if c.Mode == "internal" && c.Listen == sock {
			return true
		}
	}
	return false
}

func controlGet(p servicePaths, path string) ([]byte, string, error) {
	c, root, err := openEdgeControl(p)
	if err != nil {
		return nil, "", err
	}
	defer c.Close()
	if path == "/1.0" {
		return root, c.address, nil
	}
	b, err := c.Get(path)
	return b, c.address, err
}

func probeControl(p servicePaths) (int, string) {
	b, at, err := controlGet(p, "/1.0")
	var root struct {
		Apps int `json:"app_count"`
	}
	if err != nil || json.Unmarshal(b, &root) != nil {
		return 0, ""
	}
	return root.Apps, at
}

// localControlURL is the `control local` default, tried after the socket
// for an edge whose Caddyfile opens it. A variable so tests can point it
// at a port nothing answers on.
var localControlURL = "http://127.0.0.1:7600"

// controlReachable asks the edge's control plane for its identity: over the
// service's unix socket first (its path is this edge's alone), then over
// loopback HTTP, accepted only when the edge that answers says it also
// listens on that socket — any Janus with 'control local' answers on the
// port, and another one must not pass as this edge. Returns the count and
// the endpoint that answered, or "" when neither did.
func controlReachable(p servicePaths) bool {
	_, at := probeControl(p)
	return at != ""
}

func socketExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode()&os.ModeSocket != 0
}
