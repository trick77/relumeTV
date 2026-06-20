package webui

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLogHandler_TeesIntoHub(t *testing.T) {
	h := NewHub(16)
	base := slog.NewTextHandler(io.Discard, nil)
	log := slog.New(NewLogHandler(base, h))

	log.Info("hello", "k", "v")
	log.Warn("careful")

	evs := h.Events()
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(evs), evs)
	}
	if evs[0].Msg != "hello" || evs[0].Level != "INFO" {
		t.Fatalf("ev0 = %+v", evs[0])
	}
	if evs[1].Msg != "careful" || evs[1].Level != "WARN" {
		t.Fatalf("ev1 = %+v", evs[1])
	}
}

func TestLogHandler_SurvivesWith(t *testing.T) {
	h := NewHub(16)
	log := slog.New(NewLogHandler(slog.NewTextHandler(io.Discard, nil), h)).With("comp", "x")
	log.Info("scoped")
	if got := h.Events(); len(got) != 1 || got[0].Msg != "scoped" || got[0].Attrs != "comp=x" {
		t.Fatalf("events = %+v", got)
	}
}

// handleAttrs pushes one record with the given attrs through the log handler and
// returns the formatted Attrs string captured by the hub.
func handleAttrs(t *testing.T, msg string, args ...any) string {
	t.Helper()
	h := NewHub(8)
	lh := NewLogHandler(slog.NewTextHandler(io.Discard, nil), h)
	rec := slog.NewRecord(time.Unix(0, 0).UTC(), slog.LevelInfo, msg, 0)
	rec.Add(args...)
	if err := lh.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	evs := h.Events()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	return evs[0].Attrs
}

func TestLogHandler_SimpleAttrs(t *testing.T) {
	if got := handleAttrs(t, "forwarding failing", "failures", 3, "ok", true); got != "failures=3 ok=true" {
		t.Fatalf("Attrs = %q, want %q", got, "failures=3 ok=true")
	}
}

func TestLogHandler_QuotesValuesWithSpaces(t *testing.T) {
	if got := handleAttrs(t, "msg", "last_err", "i/o timeout"); got != `last_err="i/o timeout"` {
		t.Fatalf("Attrs = %q, want %q", got, `last_err="i/o timeout"`)
	}
}

func TestLogHandler_EscapesInnerQuotes(t *testing.T) {
	if got := handleAttrs(t, "msg", "note", `say "hi"`); got != `note="say \"hi\""` {
		t.Fatalf("Attrs = %q, want %q", got, `note="say \"hi\""`)
	}
}

func TestLogHandler_NoAttrsYieldsEmpty(t *testing.T) {
	if got := handleAttrs(t, "just a message"); got != "" {
		t.Fatalf("Attrs = %q, want empty", got)
	}
}

func TestLogHandler_PreservesOrder(t *testing.T) {
	if got := handleAttrs(t, "msg", "a", 1, "b", 2, "c", 3); got != "a=1 b=2 c=3" {
		t.Fatalf("Attrs = %q, want %q", got, "a=1 b=2 c=3")
	}
}

func TestLogHandler_CapsLongValue(t *testing.T) {
	got := handleAttrs(t, "msg", "body", strings.Repeat("x", 1000))
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("Attrs %q should be truncated with an ellipsis", got)
	}
	if len([]rune(got)) > maxAttrsLen+1 {
		t.Fatalf("Attrs rune length = %d, want <= %d", len([]rune(got)), maxAttrsLen+1)
	}
}
