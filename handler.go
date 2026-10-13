package slogbetterstack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"log/slog"

	slogcommon "github.com/samber/slog-common"
)

const BetterstackEndpoint = "https://in.logs.betterstack.com/"

// maxErrorBodyBytes caps how much of a rejected response is quoted in an error,
// so a large HTML error page cannot flood stderr.
const maxErrorBodyBytes = 512

type Option struct {
	// log level (default: debug)
	Level slog.Leveler

	// token
	Token string
	// optional: endpoint
	Endpoint string
	// default: 10s
	Timeout time.Duration

	// optional: max records per HTTP request
	// default: 100
	BatchSize int
	// optional: max delay before a partial batch is sent
	// default: 1s
	FlushInterval time.Duration
	// optional: records buffered while a batch is in flight. Records are
	// dropped (and reported through OnError) when the queue is full.
	// default: 1000
	QueueSize int

	// optional: retries after a failed attempt (network error, 429 or 5xx)
	// default: 3
	MaxRetries int
	// optional: delay before the first retry, doubled on each following one,
	// randomized between 50% and 100% to avoid synchronized retries
	// default: 500ms
	RetryBackoff time.Duration

	// optional: called when a record cannot be delivered (marshaling failure,
	// network error, non-2xx response).
	// default: print one line on stderr
	OnError func(err error)

	// optional: customize record builder
	Converter Converter
	// optional: custom marshaler
	Marshaler func(v any) ([]byte, error)
	// optional: fetch attributes from context
	AttrFromContext []func(ctx context.Context) []slog.Attr

	// optional: see slog.HandlerOptions
	AddSource   bool
	ReplaceAttr func(groups []string, a slog.Attr) slog.Attr
}

// NewBetterstackHandler starts a background worker. Call Close before the
// process exits, or queued records are lost.
func (o Option) NewBetterstackHandler() *BetterstackHandler {
	if o.Level == nil {
		o.Level = slog.LevelDebug
	}

	if o.Token == "" {
		panic("missing Betterstack token")
	}

	if o.Endpoint == "" {
		o.Endpoint = BetterstackEndpoint
	}

	if o.Timeout == 0 {
		o.Timeout = 10 * time.Second
	}

	if o.OnError == nil {
		o.OnError = defaultOnError
	}

	if o.BatchSize <= 0 {
		o.BatchSize = defaultBatchSize
	}

	if o.FlushInterval <= 0 {
		o.FlushInterval = defaultFlushInterval
	}

	if o.QueueSize <= 0 {
		o.QueueSize = defaultQueueSize
	}

	if o.MaxRetries == 0 {
		o.MaxRetries = defaultMaxRetries
	}

	if o.RetryBackoff <= 0 {
		o.RetryBackoff = defaultRetryBackoff
	}

	if o.Converter == nil {
		o.Converter = DefaultConverter
	}

	if o.Marshaler == nil {
		o.Marshaler = json.Marshal
	}

	if o.AttrFromContext == nil {
		o.AttrFromContext = []func(ctx context.Context) []slog.Attr{}
	}

	return &BetterstackHandler{
		option:     o,
		dispatcher: newDispatcher(o),
		attrs:      []slog.Attr{},
		groups:     []string{},
	}
}

var _ slog.Handler = (*BetterstackHandler)(nil)

type BetterstackHandler struct {
	option     Option
	dispatcher *dispatcher
	attrs      []slog.Attr
	groups     []string
}

func (h *BetterstackHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.option.Level.Level()
}

func (h *BetterstackHandler) Handle(ctx context.Context, record slog.Record) error {
	fromContext := slogcommon.ContextExtractor(ctx, h.option.AttrFromContext)
	payload := h.option.Converter(h.option.AddSource, h.option.ReplaceAttr, append(h.attrs, fromContext...), h.groups, &record)

	h.dispatcher.enqueue(payload)

	return nil
}

// Close stops accepting records and delivers the ones still queued. It is safe
// to call more than once, and it covers handlers derived with WithAttrs or
// WithGroup. Delivery failures are reported through OnError.
func (h *BetterstackHandler) Close() error {
	h.dispatcher.close()
	return nil
}

func (h *BetterstackHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &BetterstackHandler{
		option:     h.option,
		dispatcher: h.dispatcher,
		attrs:      slogcommon.AppendAttrsToGroup(h.groups, h.attrs, attrs...),
		groups:     h.groups,
	}
}

func (h *BetterstackHandler) WithGroup(name string) slog.Handler {
	// https://cs.opensource.google/go/x/exp/+/46b07846:slog/handler.go;l=247
	if name == "" {
		return h
	}

	return &BetterstackHandler{
		option:     h.option,
		dispatcher: h.dispatcher,
		attrs:      h.attrs,
		groups:     append(h.groups, name),
	}
}

func send(endpoint string, token string, timeout time.Duration, marshaler func(v any) ([]byte, error), payload []map[string]any) error {
	client := http.Client{
		Timeout: time.Duration(10) * time.Second,
	}

	json, err := marshaler(payload)
	if err != nil {
		return &permanentError{err}
	}

	body := bytes.NewBuffer(json)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// @TODO: maintain a pool of tcp connections
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, body)
	if err != nil {
		return &permanentError{err}
	}

	req.Header.Add("authorization", `Bearer `+token)
	req.Header.Add("content-type", `application/json`)
	req.Header.Add("user-agent", name)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// best effort: the status code alone is already actionable
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return &statusError{code: resp.StatusCode, body: string(bytes.TrimSpace(snippet))}
	}

	return nil
}

// permanentError marks a failure that a retry cannot fix.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// statusError is a non-2xx response.
type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("betterstack rejected logs: status %d: %s", e.code, e.body)
}

// retryable reports whether sending the same batch again can succeed: network
// errors, throttling and server errors. Other 4xx (bad token, bad payload) fail the same way every time.
func retryable(err error) bool {
	var permanent *permanentError
	if errors.As(err, &permanent) {
		return false
	}

	var status *statusError
	if errors.As(err, &status) {
		return status.code == http.StatusTooManyRequests || status.code >= 500
	}

	return true
}

func defaultOnError(err error) {
	fmt.Fprintf(os.Stderr, "slog-betterstack: %v\n", err)
}
