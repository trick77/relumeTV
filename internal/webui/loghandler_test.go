package webui

import (
	"io"
	"log/slog"
	"testing"
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
	if got := h.Events(); len(got) != 1 || got[0].Msg != "scoped" {
		t.Fatalf("events = %+v", got)
	}
}
