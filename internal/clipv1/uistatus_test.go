package clipv1

import (
	"io"
	"log/slog"
	"testing"

	"github.com/trick77/relume-tv/internal/config"
)

func newUIServer() *Server {
	cfg := &config.Config{ApiUsers: map[string]*config.ApiUser{}}
	return New(cfg, "127.0.0.1", 80, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestUIStatus_RestByDefault(t *testing.T) {
	s := newUIServer()
	mode, dtlsUp, fallback := s.UIStatus()
	if mode != "rest" || dtlsUp || fallback {
		t.Fatalf("got mode=%q dtlsUp=%v fallback=%v", mode, dtlsUp, fallback)
	}
}

func TestUIStatus_EntertainmentStreamUp(t *testing.T) {
	s := newUIServer()
	s.EntertainmentMode = true
	s.MarkDTLSStreamUp()
	mode, dtlsUp, fallback := s.UIStatus()
	if mode != "entertainment" || !dtlsUp || fallback {
		t.Fatalf("got mode=%q dtlsUp=%v fallback=%v", mode, dtlsUp, fallback)
	}
}

func TestLightsV1Snapshot_FalseWithoutProvider(t *testing.T) {
	s := newUIServer()
	if _, ok := s.LightsV1Snapshot(); ok {
		t.Fatal("expected ok=false with no provider")
	}
	if _, ok := s.UUIDForV1("1"); ok {
		t.Fatal("expected ok=false with no provider")
	}
}
