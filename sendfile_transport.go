package janus

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// sendfileTransport preserves net/http's ordinary transparent-gzip behavior
// while leaving a final client-bound X-Sendfile response untouched: its
// Content-Encoding describes the selected file, not the discarded
// instruction body.
type sendfileTransport struct {
	base http.RoundTripper
}

func (t sendfileTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	st := attemptOf(r.Context())
	requestedGzip := st != nil && st.autoGzip
	resp, err := t.base.RoundTrip(r)
	if err != nil || !requestedGzip || !strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		return resp, err
	}
	_, sendfile := headerValues(resp.Header, sendfileHeader)
	if sendfile && ringClassOf(r.Context()) != ringClassBridge {
		return resp, nil
	}
	resp.Body = &sendfileGzipBody{body: resp.Body}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp, nil
}

type sendfileGzipBody struct {
	body io.ReadCloser
	once sync.Once
	zr   *gzip.Reader
	err  error
}

func (b *sendfileGzipBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		b.zr, b.err = gzip.NewReader(b.body)
	})
	if b.err != nil {
		return 0, b.err
	}
	return b.zr.Read(p)
}

func (b *sendfileGzipBody) Close() error {
	if b.zr != nil {
		_ = b.zr.Close()
	}
	return b.body.Close()
}
