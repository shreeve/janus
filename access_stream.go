package janus

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type accessStatus struct {
	ProvisionedEncoders uint64             `json:"provisioned_encoders"`
	Registrations       uint64             `json:"registrations"`
	Subscribers         uint64             `json:"subscribers"`
	Caps                accessStatusCaps   `json:"caps"`
	Counters            accessStatusCounts `json:"counters"`
}

type accessStatusCaps struct {
	SubscribersPerRegistration int `json:"subscribers_per_registration"`
	SubscribersPerProcess      int `json:"subscribers_per_process"`
	QueueEvents                int `json:"queue_events"`
	LineBytes                  int `json:"line_bytes"`
	HeartbeatSeconds           int `json:"heartbeat_seconds"`
	WriteDeadlineSeconds       int `json:"write_deadline_seconds"`
}

type accessStatusCounts struct {
	Published           uint64 `json:"published"`
	SubscriberOverflows uint64 `json:"subscriber_overflows"`
	StreamOpens         uint64 `json:"stream_opens"`
	StreamCloses        uint64 `json:"stream_closes"`
	WriteTimeouts       uint64 `json:"write_timeouts"`
	InvariantFailures   uint64 `json:"invariant_failures"`
}

func (a *App) handleAccessStatus(w http.ResponseWriter, _ *http.Request) {
	b := a.access
	if b == nil {
		writeJSON(w, http.StatusOK, accessStatus{Caps: accessCaps()})
		return
	}
	b.accountingMu.Lock()
	encoders, registrations, subscribers := b.encoders, uint64(len(b.states)), b.subscribers
	b.accountingMu.Unlock()
	writeJSON(w, http.StatusOK, accessStatus{
		ProvisionedEncoders: encoders, Registrations: registrations, Subscribers: subscribers,
		Caps: accessCaps(),
		Counters: accessStatusCounts{
			Published: b.counters.published.Load(), SubscriberOverflows: b.counters.subscriberOverflow.Load(),
			StreamOpens: b.counters.streamOpens.Load(), StreamCloses: b.counters.streamCloses.Load(),
			WriteTimeouts: b.counters.writeTimeouts.Load(), InvariantFailures: b.counters.invariantFailures.Load(),
		},
	})
}

func accessCaps() accessStatusCaps {
	return accessStatusCaps{
		SubscribersPerRegistration: accessSubscribersRegistration,
		SubscribersPerProcess:      accessSubscribersProcess,
		QueueEvents:                accessQueueEvents, LineBytes: accessLineBytes,
		HeartbeatSeconds:     int(accessHeartbeat / time.Second),
		WriteDeadlineSeconds: int(accessWriteDeadline / time.Second),
	}
}

func parseAccessAfter(raw string) (uint64, error) {
	if !strings.HasPrefix(raw, "after=") {
		return 0, errors.New("query must be exactly after=<canonical uint64>")
	}
	value := strings.TrimPrefix(raw, "after=")
	if value == "" || value != "0" && value[0] == '0' {
		return 0, errors.New("after must be canonical unsigned decimal")
	}
	for _, c := range []byte(value) {
		if c < '0' || c > '9' {
			return 0, errors.New("after must be canonical unsigned decimal")
		}
	}
	after, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, errors.New("after is outside uint64")
	}
	return after, nil
}

func bodylessAccessRequest(r *http.Request) bool {
	return r.ContentLength == 0 && len(r.TransferEncoding) == 0
}

func hasFlusher(w http.ResponseWriter) bool {
	for {
		if _, ok := w.(http.Flusher); ok {
			return true
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
}

func (a *App) handleAccessStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeAPIError(w, &apiError{Status: http.StatusMethodNotAllowed, Msg: "method must be GET"})
		return
	}
	if !bodylessAccessRequest(r) {
		writeAPIError(w, errBadRequest("access stream takes no body"))
		return
	}
	after, err := parseAccessAfter(r.URL.RawQuery)
	if err != nil {
		writeAPIError(w, errBadRequest("%v", err))
		return
	}
	id := r.PathValue("id")
	reg := a.appsRegistry()
	reg.mu.Lock()
	rec := reg.apps[id]
	if rec == nil || rec.access == nil {
		reg.mu.Unlock()
		writeAPIError(w, errUnknownApp(id))
		return
	}
	st := rec.access
	st.mu.Lock()
	if st.tombstone {
		st.mu.Unlock()
		reg.mu.Unlock()
		writeAPIError(w, errUnknownApp(id))
		return
	}
	if after > st.head {
		head := st.head
		st.mu.Unlock()
		reg.mu.Unlock()
		writeAPIError(w, &apiError{Status: http.StatusConflict, Msg: fmt.Sprintf("after %d exceeds head %d", after, head)})
		return
	}
	if len(st.subscribers) >= accessSubscribersRegistration {
		st.mu.Unlock()
		reg.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		writeAPIError(w, &apiError{Status: http.StatusTooManyRequests, Msg: "registration access subscriber cap reached"})
		return
	}
	st.bridge.accountingMu.Lock()
	if st.bridge.subscribers >= accessSubscribersProcess {
		st.bridge.accountingMu.Unlock()
		st.mu.Unlock()
		reg.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		writeAPIError(w, &apiError{Status: http.StatusTooManyRequests, Msg: "process access subscriber cap reached"})
		return
	}
	if !hasFlusher(w) {
		st.bridge.accountingMu.Unlock()
		st.mu.Unlock()
		reg.mu.Unlock()
		writeAPIError(w, &apiError{Status: http.StatusInternalServerError, Msg: "response writer does not support flush"})
		return
	}
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(accessWriteDeadline)); err != nil {
		st.bridge.accountingMu.Unlock()
		st.mu.Unlock()
		reg.mu.Unlock()
		writeAPIError(w, &apiError{Status: http.StatusInternalServerError, Msg: "response writer does not support write deadlines"})
		return
	}
	sub := &accessSubscriber{
		state: st, lines: make(chan *accessLine, accessQueueEvents), accounted: after, done: make(chan struct{}),
		controller: controller,
	}
	head, appID, appName := st.head, rec.ID, rec.Name
	st.subscribers[sub] = struct{}{}
	st.bridge.subscribers++
	st.bridge.counters.streamOpens.Add(1)
	st.bridge.accountingMu.Unlock()
	st.mu.Unlock()
	reg.mu.Unlock()

	a.accessStreamsMu.Lock()
	if a.accessStopping {
		a.accessStreamsMu.Unlock()
		a.detachAccessSubscriber(sub, "generation_stop")
		writeAPIError(w, &apiError{Status: http.StatusServiceUnavailable, Msg: "control generation is stopping"})
		return
	}
	a.accessStreams[sub] = struct{}{}
	a.accessStreamsWG.Add(1)
	a.accessStreamsMu.Unlock()
	defer func() {
		a.detachAccessSubscriber(sub, "")
		a.accessStreamsMu.Lock()
		delete(a.accessStreams, sub)
		a.accessStreamsMu.Unlock()
		a.accessStreamsWG.Done()
	}()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	hello := []byte(fmt.Sprintf(
		"{\"v\":1,\"type\":\"hello\",\"app_id\":%q,\"app_name\":%q,\"after\":%q,\"head\":%q,\"heartbeat_seconds\":15,\"max_line_bytes\":8192}\n",
		appID, appName, strconv.FormatUint(after, 10), strconv.FormatUint(head, 10)))
	if !a.writeAccessLine(controller, w, hello) {
		return
	}
	if after < head {
		gap := accessGapLine(appID, after+1, head, head, "no_replay")
		if !a.writeAccessLine(controller, w, gap) {
			return
		}
		sub.accounted = head
	}

	ticker := time.NewTicker(accessHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case line := <-sub.lines:
			if !a.writeQueuedAccess(controller, w, sub, line, appID) {
				return
			}
		case <-ticker.C:
			if !a.writeTrailingGap(controller, w, sub, appID) {
				return
			}
			st.mu.Lock()
			head = st.head
			st.mu.Unlock()
			heartbeat := []byte(fmt.Sprintf(
				"{\"v\":1,\"type\":\"heartbeat\",\"app_id\":%q,\"head\":%q,\"timestamp\":%q}\n",
				appID, strconv.FormatUint(head, 10), time.Now().UTC().Format(time.RFC3339Nano)))
			if !a.writeAccessLine(controller, w, heartbeat) {
				return
			}
		case <-sub.done:
			_ = a.writeTrailingGap(controller, w, sub, appID)
			st.mu.Lock()
			head = st.head
			st.mu.Unlock()
			reason := sub.reason
			if reason != "" {
				closed := []byte(fmt.Sprintf(
					"{\"v\":1,\"type\":\"closed\",\"app_id\":%q,\"head\":%q,\"timestamp\":%q,\"reason\":%q}\n",
					appID, strconv.FormatUint(head, 10), time.Now().UTC().Format(time.RFC3339Nano), reason))
				_ = a.writeAccessLine(controller, w, closed)
			}
			return
		case <-r.Context().Done():
			return
		}
	}
}

func accessGapLine(appID string, from, through, head uint64, reason string) []byte {
	return []byte(fmt.Sprintf(
		"{\"v\":1,\"type\":\"gap\",\"app_id\":%q,\"from\":%q,\"through\":%q,\"head\":%q,\"reason\":%q}\n",
		appID, strconv.FormatUint(from, 10), strconv.FormatUint(through, 10),
		strconv.FormatUint(head, 10), reason))
}

func (a *App) writeQueuedAccess(controller *http.ResponseController, w http.ResponseWriter, sub *accessSubscriber, line *accessLine, appID string) bool {
	sub.state.mu.Lock()
	dropped := sub.droppedThrough
	head := sub.state.head
	sub.state.mu.Unlock()
	if line.sequence > sub.accounted+1 {
		through := line.sequence - 1
		if through > dropped {
			return false
		}
		if !a.writeAccessLine(controller, w, accessGapLine(appID, sub.accounted+1, through, head, "overflow")) {
			return false
		}
		sub.accounted = through
	}
	if line.sequence != sub.accounted+1 || !a.writeAccessLine(controller, w, line.data) {
		return false
	}
	sub.accounted = line.sequence
	return true
}

func (a *App) writeTrailingGap(controller *http.ResponseController, w http.ResponseWriter, sub *accessSubscriber, appID string) bool {
	pending := len(sub.lines)
	for range pending {
		line := <-sub.lines
		if !a.writeQueuedAccess(controller, w, sub, line, appID) {
			return false
		}
	}
	sub.state.mu.Lock()
	if len(sub.lines) != 0 {
		sub.state.mu.Unlock()
		return true
	}
	dropped, head := sub.droppedThrough, sub.state.head
	sub.state.mu.Unlock()
	if dropped > sub.accounted {
		if !a.writeAccessLine(controller, w, accessGapLine(appID, sub.accounted+1, dropped, head, "overflow")) {
			return false
		}
		sub.accounted = dropped
	}
	return true
}

func (a *App) writeAccessLine(controller *http.ResponseController, w http.ResponseWriter, line []byte) bool {
	deadline := time.Now().Add(accessWriteDeadline)
	a.accessStreamsMu.Lock()
	if !a.accessStopDeadline.IsZero() && a.accessStopDeadline.Before(deadline) {
		deadline = a.accessStopDeadline
	}
	a.accessStreamsMu.Unlock()
	if err := controller.SetWriteDeadline(deadline); err != nil {
		return false
	}
	if _, err := w.Write(line); err != nil {
		a.classifyAccessWriteError(err)
		return false
	}
	if err := controller.Flush(); err != nil {
		a.classifyAccessWriteError(err)
		return false
	}
	return true
}

func (a *App) classifyAccessWriteError(err error) {
	if errors.Is(err, os.ErrDeadlineExceeded) && a.access != nil {
		a.access.counters.writeTimeouts.Add(1)
	}
}

func (a *App) detachAccessSubscriber(sub *accessSubscriber, reason string) {
	st := sub.state
	st.mu.Lock()
	if sub.detached {
		st.mu.Unlock()
		return
	}
	sub.detached = true
	sub.reason = reason
	delete(st.subscribers, sub)
	st.bridge.accountingMu.Lock()
	st.bridge.subscribers--
	st.bridge.counters.streamCloses.Add(1)
	st.bridge.accountingMu.Unlock()
	st.mu.Unlock()
	if reason != "" {
		close(sub.done)
	}
}
