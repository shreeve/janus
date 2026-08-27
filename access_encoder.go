package janus

import (
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	caddylogging "github.com/caddyserver/caddy/v2/modules/logging"
	"go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

func init() {
	caddy.RegisterModule(AccessEncoder{})
}

// AccessEncoder preserves Caddy's JSON access log while publishing the
// bounded operator stream for entries carrying Janus request facts.
type AccessEncoder struct {
	caddylogging.LogEncoderConfig

	wrapped zapcore.Encoder
	bridge  *accessBridge
	logger  *zap.Logger
	request *httpRequestCapture
	cleaned bool
}

type httpRequestCapture struct {
	request *caddyhttp.LoggableHTTPRequest
}

func (AccessEncoder) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.logging.encoders.janus",
		New: func() caddy.Module { return new(AccessEncoder) },
	}
}

func (e *AccessEncoder) Provision(ctx caddy.Context) error {
	e.logger = ctx.Logger()
	bridge, err := acquireAccessBridge(e.logger)
	if err != nil {
		return err
	}
	e.bridge = bridge
	e.bridge.accountingMu.Lock()
	e.bridge.encoders++
	e.bridge.accountingMu.Unlock()
	e.wrapped = zapcore.NewJSONEncoder(e.LogEncoderConfig.ZapcoreEncoderConfig())
	return nil
}

func (e *AccessEncoder) Cleanup() error {
	if e.bridge == nil || e.cleaned {
		return nil
	}
	e.cleaned = true
	e.bridge.accountingMu.Lock()
	e.bridge.encoders--
	e.bridge.accountingMu.Unlock()
	e.bridge = nil
	return releaseAccessBridge()
}

func (e *AccessEncoder) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	if d.NextArg() {
		return d.ArgErr()
	}
	return e.LogEncoderConfig.UnmarshalCaddyfile(d)
}

func (e *AccessEncoder) Clone() zapcore.Encoder {
	clone := *e
	clone.wrapped = e.wrapped.Clone()
	if e.request != nil {
		captured := *e.request
		if e.request.request != nil {
			req := *e.request.request
			captured.request = &req
		}
		clone.request = &captured
	}
	return &clone
}

func (e *AccessEncoder) AddObject(key string, marshaler zapcore.ObjectMarshaler) error {
	if key == "request" {
		switch request := marshaler.(type) {
		case caddyhttp.LoggableHTTPRequest:
			value := request
			e.request = &httpRequestCapture{request: &value}
		case *caddyhttp.LoggableHTTPRequest:
			if request != nil {
				value := *request
				e.request = &httpRequestCapture{request: &value}
			}
		}
	}
	return e.wrapped.AddObject(key, marshaler)
}

func (e *AccessEncoder) EncodeEntry(entry zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	var facts *accessFacts
	reserved := 0
	malformed := false
	for i := range fields {
		if fields[i].Key != accessFactsKey {
			continue
		}
		reserved++
		if fields[i].Type != zapcore.SkipType {
			malformed = true
			continue
		}
		value, ok := fields[i].Interface.(*accessFacts)
		if !ok || value == nil {
			malformed = true
			continue
		}
		facts = value
	}
	if reserved > 1 || reserved == 1 && (malformed || facts == nil) {
		e.invariant(entry, facts, "malformed or duplicate access facts sentinel")
		return e.wrapped.EncodeEntry(entry, fields)
	}
	if reserved == 0 || facts == nil {
		return e.wrapped.EncodeEntry(entry, fields)
	}

	facts.mu.Lock()
	hasOwner := facts.owner.state != nil
	state := facts.owner.state
	facts.mu.Unlock()
	if !hasOwner {
		return e.wrapped.EncodeEntry(entry, fields)
	}
	if e.request == nil || e.request.request == nil || e.request.request.Request == nil {
		e.invariant(entry, facts, "access facts sentinel has no captured request")
		return e.wrapped.EncodeEntry(entry, fields)
	}
	status, _ := integerField(fields, "status")
	size, _ := integerField(fields, "size")
	facts.mu.Lock()
	if facts.published.Load() {
		conflict := facts.completionSet &&
			(facts.completionStatus != status || facts.completionSize != size)
		facts.mu.Unlock()
		if conflict {
			e.invariant(entry, facts, "duplicate publication carries conflicting completion facts")
		}
	} else {
		facts.published.Store(true)
		facts.completionSet = true
		facts.completionStatus = status
		facts.completionSize = size
		facts.mu.Unlock()
		request := e.request.request.Request
		if !state.observed() {
			if validationErr := validateAccessEvent(facts, request, fields); validationErr != nil {
				e.invariant(entry, facts, validationErr.Error())
				return e.wrapped.EncodeEntry(entry, fields)
			}
			if state.publishUnobserved() {
				return e.wrapped.EncodeEntry(entry, fields)
			}
		}
		event, eventErr := buildAccessEvent(facts, request, entry, fields, 0)
		if eventErr != nil {
			e.invariant(entry, facts, eventErr.Error())
			return e.wrapped.EncodeEntry(entry, fields)
		}
		state.publish(func(sequence uint64) ([]byte, error) {
			event.Sequence = strconv.FormatUint(sequence, 10)
			return encodeAccessEvent(event)
		})
	}
	return e.wrapped.EncodeEntry(entry, fields)
}

func (e *AccessEncoder) invariant(entry zapcore.Entry, facts *accessFacts, message string) {
	if e.bridge != nil {
		e.bridge.counters.invariantFailures.Add(1)
	}
	fields := []zap.Field{zap.String("logger", entry.LoggerName), zap.String("entry", entry.Message)}
	if facts != nil {
		facts.mu.Lock()
		fields = append(fields, zap.String("request_id", facts.requestID), zap.String("app_id", facts.owner.id))
		facts.mu.Unlock()
	}
	if e.logger != nil {
		e.logger.Error("janus access invariant failure", append(fields, zap.String("reason", message))...)
	}
}

func (e *AccessEncoder) OpenNamespace(key string) { e.wrapped.OpenNamespace(key) }
func (e *AccessEncoder) AddArray(key string, value zapcore.ArrayMarshaler) error {
	return e.wrapped.AddArray(key, value)
}
func (e *AccessEncoder) AddBinary(key string, value []byte)     { e.wrapped.AddBinary(key, value) }
func (e *AccessEncoder) AddByteString(key string, value []byte) { e.wrapped.AddByteString(key, value) }
func (e *AccessEncoder) AddBool(key string, value bool)         { e.wrapped.AddBool(key, value) }
func (e *AccessEncoder) AddComplex128(key string, value complex128) {
	e.wrapped.AddComplex128(key, value)
}
func (e *AccessEncoder) AddComplex64(key string, value complex64) { e.wrapped.AddComplex64(key, value) }
func (e *AccessEncoder) AddDuration(key string, value time.Duration) {
	e.wrapped.AddDuration(key, value)
}
func (e *AccessEncoder) AddFloat64(key string, value float64) { e.wrapped.AddFloat64(key, value) }
func (e *AccessEncoder) AddFloat32(key string, value float32) { e.wrapped.AddFloat32(key, value) }
func (e *AccessEncoder) AddInt(key string, value int)         { e.wrapped.AddInt(key, value) }
func (e *AccessEncoder) AddInt64(key string, value int64)     { e.wrapped.AddInt64(key, value) }
func (e *AccessEncoder) AddInt32(key string, value int32)     { e.wrapped.AddInt32(key, value) }
func (e *AccessEncoder) AddInt16(key string, value int16)     { e.wrapped.AddInt16(key, value) }
func (e *AccessEncoder) AddInt8(key string, value int8)       { e.wrapped.AddInt8(key, value) }
func (e *AccessEncoder) AddReflected(key string, value any) error {
	return e.wrapped.AddReflected(key, value)
}
func (e *AccessEncoder) AddString(key, value string)          { e.wrapped.AddString(key, value) }
func (e *AccessEncoder) AddTime(key string, value time.Time)  { e.wrapped.AddTime(key, value) }
func (e *AccessEncoder) AddUint(key string, value uint)       { e.wrapped.AddUint(key, value) }
func (e *AccessEncoder) AddUint64(key string, value uint64)   { e.wrapped.AddUint64(key, value) }
func (e *AccessEncoder) AddUint32(key string, value uint32)   { e.wrapped.AddUint32(key, value) }
func (e *AccessEncoder) AddUint16(key string, value uint16)   { e.wrapped.AddUint16(key, value) }
func (e *AccessEncoder) AddUint8(key string, value uint8)     { e.wrapped.AddUint8(key, value) }
func (e *AccessEncoder) AddUintptr(key string, value uintptr) { e.wrapped.AddUintptr(key, value) }

var (
	_ caddy.Module          = (*AccessEncoder)(nil)
	_ caddy.Provisioner     = (*AccessEncoder)(nil)
	_ caddy.CleanerUpper    = (*AccessEncoder)(nil)
	_ caddyfile.Unmarshaler = (*AccessEncoder)(nil)
	_ zapcore.Encoder       = (*AccessEncoder)(nil)
)
