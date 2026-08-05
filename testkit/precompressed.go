package main

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// cmdPrecompress writes the three browser-facing sidecars Janus supports for
// one canonical fixture file. It keeps test.sh self-contained across hosts
// that do not install brotli or zstd command-line tools.
func cmdPrecompress(args []string) {
	fs := flag.NewFlagSet("precompress", flag.ContinueOnError)
	input := fs.String("input", "", "canonical input file")
	if err := fs.Parse(args); err != nil || *input == "" || fs.NArg() != 0 {
		die("usage: testkit precompress --input FILE")
	}
	data, err := os.ReadFile(*input)
	if err != nil {
		die("testkit precompress: %v", err)
	}

	var br bytes.Buffer
	bw := brotli.NewWriter(&br)
	if _, err := bw.Write(data); err != nil {
		die("testkit precompress br: %v", err)
	}
	if err := bw.Close(); err != nil {
		die("testkit precompress br: %v", err)
	}
	writeSidecar(*input+".br", br.Bytes())

	var zs bytes.Buffer
	zw, err := zstd.NewWriter(&zs)
	if err != nil {
		die("testkit precompress zstd: %v", err)
	}
	if _, err := zw.Write(data); err != nil {
		die("testkit precompress zstd: %v", err)
	}
	if err := zw.Close(); err != nil {
		die("testkit precompress zstd: %v", err)
	}
	writeSidecar(*input+".zst", zs.Bytes())

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(data); err != nil {
		die("testkit precompress gzip: %v", err)
	}
	if err := gw.Close(); err != nil {
		die("testkit precompress gzip: %v", err)
	}
	writeSidecar(*input+".gz", gz.Bytes())
}

func writeSidecar(name string, data []byte) {
	if err := os.WriteFile(name, data, 0o644); err != nil {
		die("testkit precompress: %v", err)
	}
}

// cmdFetch is the acceptance client's transparent decompression proof. It
// requests one coding set, decodes the selected representation, and writes
// only the application bytes to stdout.
func cmdFetch(args []string) {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	caPath := fs.String("ca", "", "PEM certificate authority")
	accept := fs.String("accept", "", "Accept-Encoding value")
	media := fs.String("media", "", "optional Accept value")
	if err := fs.Parse(args); err != nil || *caPath == "" || *accept == "" || fs.NArg() != 1 {
		die("usage: testkit fetch --ca FILE --accept CODINGS URL")
	}
	pem, err := os.ReadFile(*caPath)
	if err != nil {
		die("testkit fetch: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		die("testkit fetch: CA file contains no certificates")
	}
	transport := &http.Transport{
		DisableCompression: true,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		},
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, fs.Arg(0), nil)
	if err != nil {
		die("testkit fetch: %v", err)
	}
	req.Header.Set("Accept-Encoding", *accept)
	if *media != "" {
		req.Header.Set("Accept", *media)
	}
	resp, err := client.Do(req)
	if err != nil {
		die("testkit fetch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		die("testkit fetch: status %d", resp.StatusCode)
	}

	var reader io.Reader = resp.Body
	var closeReader io.Closer
	switch resp.Header.Get("Content-Encoding") {
	case "":
	case "br":
		reader = brotli.NewReader(resp.Body)
	case "zstd":
		decoder, err := zstd.NewReader(resp.Body)
		if err != nil {
			die("testkit fetch zstd: %v", err)
		}
		wrapped := decoder.IOReadCloser()
		reader, closeReader = wrapped, wrapped
	case "gzip":
		decoder, err := gzip.NewReader(resp.Body)
		if err != nil {
			die("testkit fetch gzip: %v", err)
		}
		reader, closeReader = decoder, decoder
	default:
		die("testkit fetch: unsupported Content-Encoding %q", resp.Header.Get("Content-Encoding"))
	}
	if closeReader != nil {
		defer closeReader.Close()
	}
	if _, err := io.Copy(os.Stdout, reader); err != nil {
		die("testkit fetch: %v", err)
	}
}
