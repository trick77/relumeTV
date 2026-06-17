package webui

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"time"
)

//go:embed assets/*
var assetsFS embed.FS

// Server is the optional web UI HTTP server. It is separate from the TV-facing
// clipv1 server and only created/started when -ui-port is non-zero.
type Server struct {
	addr         string
	hub          *Hub
	src          StateSource
	flash        func() error
	log          *slog.Logger
	http         *http.Server
	snapInterval time.Duration
}

// NewServer builds the UI server. flash may be nil, which disables the action.
func NewServer(addr string, hub *Hub, src StateSource, flash func() error, log *slog.Logger) *Server {
	return &Server{addr: addr, hub: hub, src: src, flash: flash, log: log, snapInterval: time.Second}
}

// runSnapshotLoop periodically publishes a fresh snapshot to the hub so connected
// dashboards update live (lights, mode, stream health, pairing state). Without
// this, an SSE client only ever sees the single snapshot built at connect time
// plus log events. The cadence is coarse on purpose — this is a status view, not
// a frame mirror.
func (s *Server) runSnapshotLoop(ctx context.Context) {
	interval := s.snapInterval
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.hub.SetSnapshot(BuildSnapshot(s.src))
		}
	}
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/actions/flash", s.handleFlash)

	sub, _ := fs.Sub(assetsFS, "assets")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(BuildSnapshot(s.src))
}

func (s *Server) handleFlash(w http.ResponseWriter, r *http.Request) {
	if s.flash == nil {
		http.Error(w, "flash action unavailable", http.StatusNotFound)
		return
	}
	// CSRF guard: reject a state-changing request whose Origin (always sent by
	// browsers on cross-origin POSTs) does not match our own origin. A missing
	// Origin (curl / direct LAN access) is allowed — that path is already part of
	// the trusted-LAN threat model; this only closes the browser/DNS-rebinding hole.
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if err := s.flash(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sameOrigin reports whether r is safe to treat as same-origin for a
// state-changing request: either no Origin header (non-browser client) or an
// Origin matching this server's own host. The UI is served over plain HTTP.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	return origin == "http://"+r.Host
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	write := func(f Frame) {
		b, _ := json.Marshal(f)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		flusher.Flush()
	}

	// Initial paint: a fresh snapshot, then the buffered event tail.
	snap := BuildSnapshot(s.src)
	write(Frame{Kind: "snapshot", Snapshot: &snap})
	for _, e := range s.hub.Events() {
		e := e
		write(Frame{Kind: "event", Event: &e})
	}

	ch, cancel := s.hub.Subscribe()
	defer cancel()
	for {
		select {
		case <-r.Context().Done():
			return
		case f, ok := <-ch:
			if !ok {
				return
			}
			write(f)
		}
	}
}

// Run serves until ctx is cancelled. It returns a non-nil error only on a real
// bind/serve failure (never http.ErrServerClosed), so the caller can log it
// without taking down the headless service.
func (s *Server) Run(ctx context.Context) error {
	s.http = &http.Server{Addr: s.addr, Handler: s.Handler()}
	go s.runSnapshotLoop(ctx)
	errc := make(chan error, 1)
	go func() {
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutCtx)
		return nil
	case err := <-errc:
		return err
	}
}
