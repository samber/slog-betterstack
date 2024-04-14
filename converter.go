package slogbetterstack

import (
	"log/slog"

	slogcommon "github.com/samber/slog-common"
)

var SourceKey = "runtime"
var ContextKey = "extra"
var ErrorKeys = []string{"error", "err"}

type Converter func(addSource bool, replaceAttr func(groups []string, a slog.Attr) slog.Attr, loggerAttr []slog.Attr, groups []string, record *slog.Record) map[string]any

func DefaultConverter(addSource bool, replaceAttr func(groups []string, a slog.Attr) slog.Attr, loggerAttr []slog.Attr, groups []string, record *slog.Record) map[string]any {
	// aggregate all attributes
	attrs := slogcommon.AppendRecordAttrsToAttrs(loggerAttr, groups, record)

	// developer formatters
	attrs = slogcommon.ReplaceError(attrs, ErrorKeys...)
	if addSource {
		attrs = append(attrs, slogcommon.Source(SourceKey, record))
	}
	attrs = slogcommon.ReplaceAttrs(replaceAttr, []string{}, attrs...)

	// handler formatter
	ctx := slogcommon.AttrsToMap(attrs...)

	payload := map[string]any{
		"logger.name":    name,
		"logger.version": version,
		"dt":             record.Time.UTC(),
		"level":          record.Level.String(),
		"message":        record.Message,
		ContextKey:       ctx,
	}

	return payload
}
