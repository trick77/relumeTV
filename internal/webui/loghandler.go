package webui

import (
	"context"
	"log/slog"
)

type logHandler struct {
	next slog.Handler
	hub  *Hub
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
	})
	return l.next.Handle(ctx, rec)
}

func (l *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logHandler{next: l.next.WithAttrs(attrs), hub: l.hub}
}

func (l *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{next: l.next.WithGroup(name), hub: l.hub}
}
