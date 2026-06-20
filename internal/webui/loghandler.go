package webui

import (
	"context"
	"log/slog"
	"strings"
)

// maxAttrsLen caps the formatted attribute string so a single event that logs a
// large value (e.g. a full JSON body) cannot blow up the UI's live tail.
const maxAttrsLen = 500

type logHandler struct {
	next  slog.Handler
	hub   *Hub
	attrs []slog.Attr // accumulated via WithAttrs, prepended to each record's attrs
}

// NewLogHandler returns a slog.Handler that delegates to next and mirrors every
// record into the hub as an Event for the UI's live tail.
func NewLogHandler(next slog.Handler, h *Hub) slog.Handler {
	return &logHandler{next: next, hub: h}
}

func (l *logHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return l.next.Enabled(ctx, lvl)
}

func (l *logHandler) Handle(ctx context.Context, rec slog.Record) error {
	l.hub.PublishEvent(Event{
		Time:  rec.Time.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Level: rec.Level.String(),
		Msg:   rec.Message,
		Attrs: l.formatAttrs(rec),
	})
	return l.next.Handle(ctx, rec)
}

func (l *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(l.attrs)+len(attrs))
	merged = append(merged, l.attrs...)
	merged = append(merged, attrs...)
	return &logHandler{next: l.next.WithAttrs(attrs), hub: l.hub, attrs: merged}
}

func (l *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{next: l.next.WithGroup(name), hub: l.hub, attrs: l.attrs}
}

// formatAttrs renders the handler's accumulated attrs followed by the record's
// attrs as a single logfmt string (e.g. `failures=3 last_err="i/o timeout"`),
// preserving the order in which they were passed to the log call.
func (l *logHandler) formatAttrs(rec slog.Record) string {
	var b strings.Builder
	write := func(a slog.Attr) bool {
		a.Value = a.Value.Resolve()
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(quoteValue(a.Value.String()))
		return true
	}
	for _, a := range l.attrs {
		write(a)
	}
	rec.Attrs(func(a slog.Attr) bool { return write(a) })

	s := b.String()
	if len(s) > maxAttrsLen {
		// Truncate on a rune boundary so a multi-byte value can't be split.
		r := []rune(s)
		if len(r) > maxAttrsLen {
			r = r[:maxAttrsLen]
		}
		s = string(r) + "…"
	}
	return s
}

// quoteValue wraps a logfmt value in double quotes when it is empty or contains
// whitespace, a quote, or an equals sign; inner quotes are escaped.
func quoteValue(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\n\"=") {
		return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
	}
	return v
}
