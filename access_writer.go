package janus

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
)

// accessResponseWriter observes ownership of failed client writes while
// preserving the optional interfaces Caddy and Gorilla discover through
// ResponseController.
type accessResponseWriter struct {
	http.ResponseWriter
	request *http.Request
	facts   *accessFacts
}

func (w *accessResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *accessResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err != nil {
		w.record(err)
	}
	return n, err
}

func (w *accessResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	n, err := io.Copy(w.ResponseWriter, r)
	if err != nil {
		w.record(err)
	}
	return n, err
}

func (w *accessResponseWriter) Flush() {
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		w.record(err)
	}
}

func (w *accessResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		w.record(err)
	}
	return conn, rw, err
}

func (w *accessResponseWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	err := pusher.Push(target, options)
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		w.record(err)
	}
	return err
}

func (w *accessResponseWriter) record(_ error) {
	if w.facts == nil {
		return
	}
	w.facts.mu.Lock()
	if w.request.Context().Err() != nil {
		w.facts.outcome = "client_canceled"
	} else if w.facts.outcome != "upstream_aborted" {
		w.facts.outcome = "write_error"
	}
	w.facts.mu.Unlock()
}
