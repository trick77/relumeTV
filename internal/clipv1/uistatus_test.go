package clipv1

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/trick77/relume/internal/config"
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

func TestPendingTVPairing_WindowOpen(t *testing.T) {
	s := newUIServer()
	s.pairing.mu.Lock()
	s.pairing.acceptDelay = time.Minute
	s.pairing.firstPairSeen = time.Now()
	s.pairing.mu.Unlock()
	if !s.PendingTVPairing() {
		t.Fatal("expected pending pairing within the accept window")
	}
}

func TestPendingTVPairing_NoneWhenUnseen(t *testing.T) {
	s := newUIServer()
	if s.PendingTVPairing() {
		t.Fatal("expected no pending pairing before any attempt")
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
