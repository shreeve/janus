package main

// Acceptance drivers for capability 10: a UDP echo fixture and a
// WebTransport client that exchanges datagrams through the relay and
// reports what came back.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// runUDPEcho answers every datagram with its own bytes. It prints the
// bound address on stdout first, so a caller may pass port 0.
func runUDPEcho(args []string) {
	fs := flag.NewFlagSet("udp-echo", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:0", "address to bind")
	_ = fs.Parse(args)
	addr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		die("udp-echo: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		die("udp-echo: %v", err)
	}
	fmt.Printf("addr=%s\n", conn.LocalAddr())
	buf := make([]byte, 65535)
	for {
		n, peer, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		_, _ = conn.WriteToUDPAddrPort(buf[:n], peer)
	}
}

type wtReport struct {
	Status     int     `json:"status"`
	Sent       int     `json:"sent"`
	Received   int     `json:"received"`
	Mismatched int     `json:"mismatched"`
	Bytes      int     `json:"bytes"`
	P50Ms      float64 `json:"p50_ms"`
	P99Ms      float64 `json:"p99_ms"`
	Error      string  `json:"error,omitempty"`
}

// runWT dials a relay route, sends N datagrams of SIZE bytes, and waits
// for each echo before sending the next; the report is JSON on stdout.
// A non-200 status or any mismatch exits nonzero, so a shell case can
// gate on it and still read the report.
func runWT(args []string) {
	fs := flag.NewFlagSet("wt", flag.ExitOnError)
	rawURL := fs.String("url", "", "https://host/path to dial")
	ca := fs.String("ca", "", "PEM root to trust (system roots when empty)")
	origin := fs.String("origin", "", "Origin header value (none when empty)")
	resolve := fs.String("resolve", "", "ip:port to dial instead of the URL's host")
	n := fs.Int("n", 10, "datagrams to send")
	size := fs.Int("size", 1152, "bytes per datagram")
	timeout := fs.Duration("timeout", 10*time.Second, "overall deadline")
	pmtud := fs.Bool("pmtud", false, "leave path MTU discovery on (off measures the first-flight budget)")
	_ = fs.Parse(args)
	if *rawURL == "" {
		die("wt: --url is required")
	}
	rep := wtReport{}
	var sess *webtransport.Session
	exit := func() {
		if sess != nil {
			_ = sess.CloseWithError(0, "done") // os.Exit runs no defers; close first
		}
		enc := json.NewEncoder(os.Stdout)
		_ = enc.Encode(rep)
		if rep.Error != "" || rep.Status != http.StatusOK || rep.Mismatched != 0 || rep.Received != rep.Sent {
			os.Exit(1)
		}
		os.Exit(0)
	}
	u, err := url.Parse(*rawURL)
	if err != nil {
		die("wt: %v", err)
	}
	// The library does not fill the server name from the URL; the SNI the
	// relay picks its certificate by is the URL's host.
	tlsConf := &tls.Config{NextProtos: []string{http3.NextProtoH3}, MinVersion: tls.VersionTLS13, ServerName: u.Hostname()}
	if *ca != "" {
		pem, err := os.ReadFile(*ca)
		if err != nil {
			rep.Error = err.Error()
			exit()
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			rep.Error = "no certificate in " + *ca
			exit()
		}
		tlsConf.RootCAs = pool
	}
	d := &webtransport.Dialer{
		TLSClientConfig: tlsConf,
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true, DisablePathMTUDiscovery: !*pmtud},
	}
	// The library dials the URL's host verbatim, so a URL without a port
	// needs the HTTPS default supplied here.
	dialAddr := *resolve
	if dialAddr == "" {
		dialAddr = u.Host
		if _, _, err := net.SplitHostPort(dialAddr); err != nil {
			dialAddr = net.JoinHostPort(dialAddr, "443")
		}
	}
	d.DialAddr = func(ctx context.Context, _ string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
		return quic.DialAddrEarly(ctx, dialAddr, tlsCfg, cfg)
	}
	hdr := http.Header{}
	if *origin != "" {
		hdr.Set("Origin", *origin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, s, err := d.Dial(ctx, *rawURL, hdr)
	sess = s
	if res != nil {
		rep.Status = res.StatusCode
	}
	if err != nil {
		if rep.Status == 0 {
			rep.Error = err.Error()
		}
		exit()
	}
	var rtts []float64
	for i := 0; i < *n; i++ {
		payload := make([]byte, *size)
		for j := range payload {
			payload[j] = byte(i + j)
		}
		start := time.Now()
		if err := sess.SendDatagram(payload); err != nil {
			rep.Error = fmt.Sprintf("send %d: %v", i, err)
			exit()
		}
		rep.Sent++
		got, err := sess.ReceiveDatagram(ctx)
		if err != nil {
			rep.Error = fmt.Sprintf("receive %d: %v", i, err)
			exit()
		}
		rtts = append(rtts, float64(time.Since(start).Microseconds())/1000)
		rep.Received++
		rep.Bytes += len(got)
		if string(got) != string(payload) {
			rep.Mismatched++
		}
	}
	sort.Float64s(rtts)
	if len(rtts) > 0 {
		rep.P50Ms = rtts[len(rtts)/2]
		rep.P99Ms = rtts[(len(rtts)*99)/100]
	}
	exit()
}
