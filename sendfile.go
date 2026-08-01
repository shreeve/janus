package janus

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
)

const sendfileHeader = "X-Sendfile"

var errSendfileNoOverlap = errors.New("invalid range: failed to overlap")

// applySendfile replaces a final upstream instruction with a descriptor-backed
// file response. It reports whether the response contained the instruction.
func (dp *dataPlane) applySendfile(resp *http.Response) bool {
	values, present := headerValues(resp.Header, sendfileHeader)
	if !present {
		return false
	}
	resp.Header.Del(sendfileHeader)
	if resp.Body != nil {
		_ = resp.Body.Close() // never drain an untrusted instruction body
	}

	fail := func(reason string) bool {
		dp.logger.Warn("janus rejected upstream sendfile response", zap.String("reason", reason))
		setSendfileError(resp, http.StatusBadGateway, "bad gateway\n")
		return true
	}
	if len(values) != 1 {
		return fail("instruction must have exactly one field value")
	}
	name := values[0]
	if name == "" || !utf8.ValidString(name) || strings.IndexByte(name, 0) >= 0 || !filepath.IsAbs(name) {
		return fail("instruction path must be absolute UTF-8 without NUL")
	}
	if statusBodyForbidden(resp.StatusCode) {
		return fail("instruction status cannot carry a body")
	}

	file, err := openSendfile(name)
	if err != nil {
		return fail("instruction path could not be opened")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return fail("instruction target is not a readable regular file")
	}

	header := resp.Header
	clearSendfileTransportHeaders(header)
	modtime, ok := prepareSendfileValidators(header, info)
	if !ok {
		_ = file.Close()
		return fail("instruction supplied an invalid validator")
	}

	req := resp.Request
	if req == nil {
		_ = file.Close()
		return fail("instruction response has no request")
	}
	selectionReq := req
	if resp.StatusCode != http.StatusOK {
		selectionReq = req.Clone(req.Context())
		clearSendfileSelectionHeaders(selectionReq.Header)
	}
	selected, err := selectSendfile(selectionReq, header, file, info.Size(), modtime, name)
	if err != nil {
		_ = file.Close()
		return fail("instruction response could not be selected")
	}
	if selected.status == http.StatusOK && resp.StatusCode != http.StatusOK {
		selected.status = resp.StatusCode
	}
	var body io.ReadCloser
	if selected.body == nil {
		_ = file.Close()
		body = http.NoBody
	} else {
		body = &sendfileBody{reader: selected.body, file: file}
	}
	setSendfileResponse(resp, selected.status, header, body, selected.length)
	return true
}

func headerValues(h http.Header, name string) ([]string, bool) {
	canonical := http.CanonicalHeaderKey(name)
	values, present := h[canonical]
	for k, vv := range h {
		if k == canonical || !strings.EqualFold(k, name) {
			continue
		}
		if !present {
			values, present = vv, true
			continue
		}
		combined := make([]string, 0, len(values)+len(vv))
		combined = append(combined, values...)
		values = append(combined, vv...)
	}
	return values, present
}

func statusBodyForbidden(status int) bool {
	return status >= 100 && status < 200 ||
		status == http.StatusNoContent ||
		status == http.StatusResetContent ||
		status == http.StatusNotModified ||
		status == http.StatusPartialContent ||
		status == http.StatusRequestedRangeNotSatisfiable
}

func clearSendfileTransportHeaders(h http.Header) {
	h.Del("Content-Length")
	h.Del("Content-Range")
	h.Del("Transfer-Encoding")
	h.Del("Trailer")
}

func clearSendfileSelectionHeaders(h http.Header) {
	for _, name := range []string{
		"Range", "If-Range", "If-Match", "If-None-Match",
		"If-Modified-Since", "If-Unmodified-Since",
	} {
		h.Del(name)
	}
}

func prepareSendfileValidators(h http.Header, info os.FileInfo) (time.Time, bool) {
	modtime := info.ModTime()
	if values, present := headerValues(h, "ETag"); present {
		if len(values) != 1 || !validResponseETag(values[0]) {
			return time.Time{}, false
		}
	} else {
		h.Set("ETag", fmt.Sprintf(`W/"%x-%x"`, modtime.UnixNano(), info.Size()))
	}
	if values, present := headerValues(h, "Last-Modified"); present {
		if len(values) != 1 {
			return time.Time{}, false
		}
		parsed, err := http.ParseTime(values[0])
		if err != nil {
			return time.Time{}, false
		}
		modtime = parsed
	} else if !modtime.IsZero() {
		h.Set("Last-Modified", modtime.UTC().Format(http.TimeFormat))
	}
	return modtime, true
}

func prepareSendfileRepresentation(h http.Header, file *os.File, name string) {
	if _, present := headerValues(h, "Content-Type"); !present {
		contentType := mime.TypeByExtension(filepath.Ext(name))
		if contentType == "" {
			var sniff [512]byte
			n, _ := file.ReadAt(sniff[:], 0)
			contentType = http.DetectContentType(sniff[:n])
		}
		h.Set("Content-Type", contentType)
	}
}

func validResponseETag(value string) bool {
	etag, remain := scanSendfileETag(value)
	return etag != "" && strings.TrimSpace(remain) == ""
}

type sendfileSelection struct {
	status int
	length int64
	body   io.Reader
}

func selectSendfile(r *http.Request, h http.Header, file *os.File, size int64, modtime time.Time, name string) (sendfileSelection, error) {
	status, rangeHeader := checkSendfilePreconditions(r, h, modtime)
	if status != 0 {
		if status == http.StatusNotModified {
			h.Del("Content-Type")
			h.Del("Content-Length")
			h.Del("Content-Encoding")
			if h.Get("ETag") != "" {
				h.Del("Last-Modified")
			}
		}
		return sendfileSelection{status: status, length: 0}, nil
	}
	prepareSendfileRepresentation(h, file, name)

	ranges, err := parseSendfileRange(rangeHeader, size)
	switch err {
	case nil:
	case errSendfileNoOverlap:
		if size == 0 {
			ranges = nil
			break
		}
		h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		fallthrough
	default:
		clearSendfileErrorHeaders(h)
		body := []byte(err.Error() + "\n")
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		return sendfileSelection{status: http.StatusRequestedRangeNotSatisfiable, length: int64(len(body)), body: bytes.NewReader(body)}, nil
	}
	if sumSendfileRanges(ranges) > size {
		ranges = nil
	}
	if _, present := headerValues(h, "Accept-Ranges"); !present {
		h.Set("Accept-Ranges", "bytes")
	}

	status = http.StatusOK
	length := size
	var body io.Reader = io.NewSectionReader(file, 0, size)
	switch len(ranges) {
	case 0:
	case 1:
		ra := ranges[0]
		status = http.StatusPartialContent
		length = ra.length
		body = io.NewSectionReader(file, ra.start, ra.length)
		h.Set("Content-Range", ra.contentRange(size))
	default:
		status = http.StatusPartialContent
		contentType := h.Get("Content-Type")
		var contentLength int64
		body, contentLength, err = multipartSendfileBody(file, ranges, contentType, size)
		if err != nil {
			return sendfileSelection{}, err
		}
		length = contentLength
		h.Set("Content-Type", body.(*sendfileMultipartReader).contentType)
	}
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	if r.Method == http.MethodHead {
		body = nil
	}
	return sendfileSelection{status: status, length: length, body: body}, nil
}

type sendfileBody struct {
	reader io.Reader
	file   *os.File
	closed atomic.Bool
}

func (b *sendfileBody) Read(p []byte) (int, error) {
	if b.closed.Load() {
		return 0, os.ErrClosed
	}
	return b.reader.Read(p)
}

func (b *sendfileBody) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	return b.file.Close()
}

type sendfileMultipartReader struct {
	io.Reader
	contentType string
}

func multipartSendfileBody(file *os.File, ranges []sendfileRange, contentType string, size int64) (io.Reader, int64, error) {
	var encoded bytes.Buffer
	mw := multipart.NewWriter(&encoded)
	boundary := mw.Boundary()
	var readers []io.Reader
	var total int64
	for _, ra := range ranges {
		before := encoded.Len()
		if _, err := mw.CreatePart(ra.mimeHeader(contentType, size)); err != nil {
			return nil, 0, err
		}
		prefix := bytes.Clone(encoded.Bytes()[before:])
		readers = append(readers, bytes.NewReader(prefix), io.NewSectionReader(file, ra.start, ra.length))
		total += int64(len(prefix)) + ra.length
	}
	before := encoded.Len()
	if err := mw.Close(); err != nil {
		return nil, 0, err
	}
	suffix := bytes.Clone(encoded.Bytes()[before:])
	readers = append(readers, bytes.NewReader(suffix))
	total += int64(len(suffix))
	return &sendfileMultipartReader{
		Reader:      io.MultiReader(readers...),
		contentType: "multipart/byteranges; boundary=" + boundary,
	}, total, nil
}

func setSendfileResponse(resp *http.Response, status int, h http.Header, body io.ReadCloser, length int64) {
	resp.StatusCode = status
	resp.Status = fmt.Sprintf("%d %s", status, http.StatusText(status))
	resp.Header = h
	resp.Body = body
	resp.ContentLength = length
	resp.TransferEncoding = nil
	resp.Trailer = nil
	resp.Uncompressed = false
}

func setSendfileError(resp *http.Response, status int, message string) {
	body := []byte(message)
	h := make(http.Header)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	setSendfileResponse(resp, status, h, io.NopCloser(bytes.NewReader(body)), int64(len(body)))
}

func clearSendfileErrorHeaders(h http.Header) {
	for _, name := range []string{
		"Cache-Control", "Content-Encoding", "ETag", "Last-Modified",
		"Content-Length",
	} {
		h.Del(name)
	}
}

// The validator and range helpers follow net/http ServeContent's RFC ordering
// while producing a response plan instead of writing to a ResponseWriter.

type sendfileCond int

const (
	sendfileCondNone sendfileCond = iota
	sendfileCondTrue
	sendfileCondFalse
)

func scanSendfileETag(s string) (etag, remain string) {
	s = textproto.TrimString(s)
	start := 0
	if strings.HasPrefix(s, "W/") {
		start = 2
	}
	if len(s[start:]) < 2 || s[start] != '"' {
		return "", ""
	}
	for i := start + 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == 0x21 || c >= 0x23 && c <= 0x7e || c >= 0x80:
		case c == '"':
			return s[:i+1], s[i+1:]
		default:
			return "", ""
		}
	}
	return "", ""
}

func sendfileStrongETagMatch(a, b string) bool { return a == b && a != "" && a[0] == '"' }
func sendfileWeakETagMatch(a, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
}

func checkSendfileIfMatch(r *http.Request, h http.Header) sendfileCond {
	value := r.Header.Get("If-Match")
	if value == "" {
		return sendfileCondNone
	}
	for {
		value = textproto.TrimString(value)
		if value == "" {
			break
		}
		if value[0] == ',' {
			value = value[1:]
			continue
		}
		if value[0] == '*' {
			return sendfileCondTrue
		}
		etag, remain := scanSendfileETag(value)
		if etag == "" {
			break
		}
		if sendfileStrongETagMatch(etag, h.Get("ETag")) {
			return sendfileCondTrue
		}
		value = remain
	}
	return sendfileCondFalse
}

func checkSendfileIfUnmodifiedSince(r *http.Request, modtime time.Time) sendfileCond {
	value := r.Header.Get("If-Unmodified-Since")
	if value == "" || modtime.IsZero() {
		return sendfileCondNone
	}
	t, err := http.ParseTime(value)
	if err != nil {
		return sendfileCondNone
	}
	if modtime.Truncate(time.Second).Compare(t) <= 0 {
		return sendfileCondTrue
	}
	return sendfileCondFalse
}

func checkSendfileIfNoneMatch(r *http.Request, h http.Header) sendfileCond {
	value := r.Header.Get("If-None-Match")
	if value == "" {
		return sendfileCondNone
	}
	for {
		value = textproto.TrimString(value)
		if value == "" {
			break
		}
		if value[0] == ',' {
			value = value[1:]
			continue
		}
		if value[0] == '*' {
			return sendfileCondFalse
		}
		etag, remain := scanSendfileETag(value)
		if etag == "" {
			break
		}
		if sendfileWeakETagMatch(etag, h.Get("ETag")) {
			return sendfileCondFalse
		}
		value = remain
	}
	return sendfileCondTrue
}

func checkSendfileIfModifiedSince(r *http.Request, modtime time.Time) sendfileCond {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return sendfileCondNone
	}
	value := r.Header.Get("If-Modified-Since")
	if value == "" || modtime.IsZero() {
		return sendfileCondNone
	}
	t, err := http.ParseTime(value)
	if err != nil {
		return sendfileCondNone
	}
	if modtime.Truncate(time.Second).Compare(t) <= 0 {
		return sendfileCondFalse
	}
	return sendfileCondTrue
}

func checkSendfileIfRange(r *http.Request, h http.Header, modtime time.Time) sendfileCond {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return sendfileCondNone
	}
	value := r.Header.Get("If-Range")
	if value == "" {
		return sendfileCondNone
	}
	if etag, _ := scanSendfileETag(value); etag != "" {
		if sendfileStrongETagMatch(etag, h.Get("ETag")) {
			return sendfileCondTrue
		}
		return sendfileCondFalse
	}
	t, err := http.ParseTime(value)
	if err != nil || modtime.IsZero() {
		return sendfileCondFalse
	}
	if t.Unix() == modtime.Unix() {
		return sendfileCondTrue
	}
	return sendfileCondFalse
}

func checkSendfilePreconditions(r *http.Request, h http.Header, modtime time.Time) (status int, rangeHeader string) {
	condition := checkSendfileIfMatch(r, h)
	if condition == sendfileCondNone {
		condition = checkSendfileIfUnmodifiedSince(r, modtime)
	}
	if condition == sendfileCondFalse {
		return http.StatusPreconditionFailed, ""
	}
	switch checkSendfileIfNoneMatch(r, h) {
	case sendfileCondFalse:
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			return http.StatusNotModified, ""
		}
		return http.StatusPreconditionFailed, ""
	case sendfileCondNone:
		if checkSendfileIfModifiedSince(r, modtime) == sendfileCondFalse {
			return http.StatusNotModified, ""
		}
	}
	rangeHeader = r.Header.Get("Range")
	if rangeHeader != "" && checkSendfileIfRange(r, h, modtime) == sendfileCondFalse {
		rangeHeader = ""
	}
	return 0, rangeHeader
}

type sendfileRange struct {
	start  int64
	length int64
}

func (r sendfileRange) contentRange(size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", r.start, r.start+r.length-1, size)
}

func (r sendfileRange) mimeHeader(contentType string, size int64) textproto.MIMEHeader {
	return textproto.MIMEHeader{
		"Content-Range": {r.contentRange(size)},
		"Content-Type":  {contentType},
	}
}

func parseSendfileRange(value string, size int64) ([]sendfileRange, error) {
	if value == "" {
		return nil, nil
	}
	const prefix = "bytes="
	if !strings.HasPrefix(value, prefix) {
		return nil, errors.New("invalid range")
	}
	var ranges []sendfileRange
	noOverlap := false
	for part := range strings.SplitSeq(value[len(prefix):], ",") {
		part = textproto.TrimString(part)
		if part == "" {
			continue
		}
		start, end, ok := strings.Cut(part, "-")
		if !ok {
			return nil, errors.New("invalid range")
		}
		start, end = textproto.TrimString(start), textproto.TrimString(end)
		var ra sendfileRange
		if start == "" {
			if end == "" || end[0] == '-' {
				return nil, errors.New("invalid range")
			}
			n, err := strconv.ParseInt(end, 10, 64)
			if err != nil || n < 0 {
				return nil, errors.New("invalid range")
			}
			if n > size {
				n = size
			}
			ra.start = size - n
			ra.length = size - ra.start
		} else {
			n, err := strconv.ParseInt(start, 10, 64)
			if err != nil || n < 0 {
				return nil, errors.New("invalid range")
			}
			if n >= size {
				noOverlap = true
				continue
			}
			ra.start = n
			if end == "" {
				ra.length = size - ra.start
			} else {
				n, err = strconv.ParseInt(end, 10, 64)
				if err != nil || ra.start > n {
					return nil, errors.New("invalid range")
				}
				if n >= size {
					n = size - 1
				}
				ra.length = n - ra.start + 1
			}
		}
		ranges = append(ranges, ra)
	}
	if noOverlap && len(ranges) == 0 {
		return nil, errSendfileNoOverlap
	}
	return ranges, nil
}

func sumSendfileRanges(ranges []sendfileRange) (size int64) {
	for _, ra := range ranges {
		size += ra.length
	}
	return size
}
