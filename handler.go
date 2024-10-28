package slogbetterstack

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"log/slog"

	slogcommon "github.com/samber/slog-common"
)

const BetterstackEndpoint = "https://in.logs.betterstack.com/"

type Option struct {
	// log level (default: debug)
	Level slog.Leveler

	// token
	Token   string
	Timeout time.Duration // default: 10s

	// optional: customize record builder
	Converter Converter
	// optional: custom marshaler
	Marshaler func(v any) ([]byte, error)
	// optional: fetch attributes from context
	AttrFromContext []func(ctx context.Context) []slog.Attr

	// optional: see slog.HandlerOptions
	AddSource   bool
	ReplaceAttr func(groups []string, a slog.Attr) slog.Attr

	// optional: endpoint
	Endpoint string
}

func (o Option) NewBetterstackHandler() slog.Handler {
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
		option: o,
		attrs:  []slog.Attr{},
		groups: []string{},
	}
}

var _ slog.Handler = (*BetterstackHandler)(nil)

type BetterstackHandler struct {
	option Option
	attrs  []slog.Attr
	groups []string
}

func (h *BetterstackHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.option.Level.Level()
}

func (h *BetterstackHandler) Handle(ctx context.Context, record slog.Record) error {
	fromContext := slogcommon.ContextExtractor(ctx, h.option.AttrFromContext)
	payload := h.option.Converter(h.option.AddSource, h.option.ReplaceAttr, append(h.attrs, fromContext...), h.groups, &record)

	// non-blocking
	go func() {
		// @TODO: batching ?
		_ = send(h.option.Endpoint, h.option.Token, h.option.Timeout, h.option.Marshaler, []map[string]any{payload})
	}()

	return nil
}

func (h *BetterstackHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &BetterstackHandler{
		option: h.option,
		attrs:  slogcommon.AppendAttrsToGroup(h.groups, h.attrs, attrs...),
		groups: h.groups,
	}
}

func (h *BetterstackHandler) WithGroup(name string) slog.Handler {
	// https://cs.opensource.google/go/x/exp/+/46b07846:slog/handler.go;l=247
	if name == "" {
		return h
	}

	return &BetterstackHandler{
		option: h.option,
		attrs:  h.attrs,
		groups: append(h.groups, name),
	}
}

func send(endpoint string, token string, timeout time.Duration, marshaler func(v any) ([]byte, error), payload []map[string]any) error {
	client := http.Client{
		Timeout: time.Duration(10) * time.Second,
	}

	json, err := marshaler(payload)
	if err != nil {
		return err
	}

	body := bytes.NewBuffer(json)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// @TODO: maintain a pool of tcp connections
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, body)
	if err != nil {
		return err
	}

	req.Header.Add("authorization", `Bearer `+token)
	req.Header.Add("content-type", `application/json`)
	req.Header.Add("user-agent", name)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	return nil
}
