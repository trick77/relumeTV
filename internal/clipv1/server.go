// Package clipv1 provides the CLIP-v1 HTTP interface that the Ambilight TV
// expects: /description.xml, pairing (POST /api), config and (in later
// milestones) lights/groups as well as activating the entertainment stream.
package clipv1

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/trick77/relume/internal/config"
	"github.com/trick77/relume/internal/upnp"
)

// LightProvider supplies the (already v1-translated) light list of the Bridge Pro
// and sets light states (REST fallback). It is set by the backend (M2+); if it is
// nil, the server returns empty lists (M1).
type LightProvider interface {
	// LightsV1 returns the v1 light list (key = numeric ID as a string).
	LightsV1() (map[string]any, error)
	// SetLightV1 sets the state of a light by its v1 ID.
	SetLightV1(v1id string, v1state map[string]any) error
}

// Server serves the CLIP-v1 interface.
type Server struct {
	cfg      *config.Config
	advIP    string
	httpPort int
	log      *slog.Logger
	lightsMu sync.RWMutex
	lights   LightProvider
	// Debug enables verbose request logging (User-Agent + body) — helpful for
	// analyzing the real behavior of unknown TVs.
	Debug bool
	// IdentityProfile selects experimental wire-identity compatibility tweaks.
	// Empty keeps the default; "ambilight" matches the Ambilight-specific
	// OSS emulator; "hass" matches Home Assistant emulated-hue.
	IdentityProfile string
	// DescriptionProfile selects experimental description.xml body formatting.
	// Empty keeps the default; "ambilight-reference" matches the Ambilight OSS descriptor.
	DescriptionProfile string
	// MediaServerAlias makes /description.xml match the opt-in SSDP MediaServer:1 alias.
	MediaServerAlias bool
	// MediaServerBasicBody keeps the ms1 alias URL but serves a Hue Basic descriptor body.
	MediaServerBasicBody bool
	// TVIP is the TV's IP (from -tv-ip). Pairing is auto-accepted only for the TV,
	// identified by this IP or by the Android/Dalvik Philips-TV User-Agent.
	TVIP string

	// EntProbe enables the entertainment diagnostic (RELUME_ENT_PROBE=1): the TV's
	// stream-activation PUT is confirmed with the real v1 success shape and the
	// Entertainment group reflects stream.active+owner, so the TV proceeds to open
	// the DTLS stream instead of aborting — letting the udp :2100 probe observe
	// whether it tries DTLS at all. Off keeps the legacy log-and-ack behavior.
	EntProbe bool

	// EntertainmentMode makes relume confirm the TV's stream activation for real
	// (so the TV opens the DTLS entertainment stream, which the receiver services)
	// instead of the REST-mode generic ack that keeps the TV on REST-follow. Opt-in
	// via -mode entertainment; REST stays the default.
	EntertainmentMode bool

	// ControlledLights, if set, returns the Bridge Pro light UUIDs the TV is
	// currently driving (the flash-target set). Surfaced in the activity rollup so
	// the live Ambilight light set is visible. Wired to ControlledSet by main.
	ControlledLights func() []string

	// activity accumulates the high-frequency light-state writes Ambilight sends
	// (REST control path) so they can be summarized periodically instead of
	// logging every single request. See LogActivitySummary.
	activityMu    sync.Mutex
	lightWrites   uint64
	lightsTouched map[string]struct{}
	// groupActionWrites counts PUT /groups/{id}/action writes — a second control
	// path the TV could push Ambilight frames over. Tallied so the activity Hz
	// reading cannot be faked out by frames arriving on the group endpoint.
	groupActionWrites uint64

	// stream tracks the Entertainment group's stream state under EntProbe so GET
	// /groups/1 reflects the activation the TV requested (active + owner).
	streamMu     sync.Mutex
	streamActive bool
	streamOwner  string
	// lastWriteAt is the time of the most recent Ambilight light-state write,
	// stamped in handleSetLightState (so it is independent of Debug, unlike the
	// activity counters above). The idle-off monitor reads it via LastActivity.
	lastWriteAt time.Time

	// pairMu guards firstPairSeen, the timestamp of the TV's first auto-pairing
	// attempt. New pairings are held off for pairAcceptDelay after that (returning
	// the standard 101), mirroring a real bridge waiting for the link-button tap.
	pairMu        sync.Mutex
	firstPairSeen time.Time
	// pairAcceptDelay defers auto-accepting the TV's first pairing (measured from
	// the first attempt); defaults to defaultPairAcceptDelay, overridable in tests.
	pairAcceptDelay time.Duration
}

// defaultPairAcceptDelay is how long relume defers auto-accepting the TV's first
// pairing. This is purely cosmetic — to make pairing feel natural: a real Hue
// bridge only pairs after you physically tap its link button, so accepting
// instantly would feel "off". relume always auto-accepts; the delay just mimics
// that short, expected wait. It stays well inside the TV's ~30s POST /api polling
// window. Only the FIRST pairing of a devicetype is delayed; an already-paired TV
// is served instantly from the idempotent path.
const defaultPairAcceptDelay = 10 * time.Second

// New creates the CLIP-v1 server.
func New(cfg *config.Config, advIP string, httpPort int, log *slog.Logger) *Server {
	return &Server{cfg: cfg, advIP: advIP, httpPort: httpPort, log: log, lightsTouched: map[string]struct{}{}, pairAcceptDelay: defaultPairAcceptDelay}
}

// confirmsEntertainment reports whether relume should confirm the TV's stream
// activation for real: in entertainment mode, or under the diagnostic probe.
func (s *Server) confirmsEntertainment() bool { return s.EntertainmentMode || s.EntProbe }

// SetLightProvider registers the source for the light list (Bridge Pro backend).
// Safe to call at runtime: the backend may be paired asynchronously after the
// HTTP server is already serving the TV.
func (s *Server) SetLightProvider(p LightProvider) {
	s.lightsMu.Lock()
	s.lights = p
	s.lightsMu.Unlock()
}

// lightProvider returns the current backend (may be nil until the Pro is paired).
func (s *Server) lightProvider() LightProvider {
	s.lightsMu.RLock()
	defer s.lightsMu.RUnlock()
	return s.lights
}

// lightsV1 returns the Bridge Pro lights (v1-translated), or an empty map if no
// backend is paired yet or the read fails. This is the single source of truth for
// the lights both in GET /api/{user} (full datastore) and GET /api/{user}/lights.
func (s *Server) lightsV1() map[string]any {
	lp := s.lightProvider()
	if lp == nil {
		return map[string]any{}
	}
	lights, err := lp.LightsV1()
	if err != nil {
		s.log.Warn("reading lights from bridge pro", "err", err)
		return map[string]any{}
	}
	return lights
}

// Handler returns the HTTP handler (routing) for the server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /description.xml", s.handleDescription)
	mux.HandleFunc("POST /api", s.handlePairing)
	mux.HandleFunc("POST /api/", s.handlePairing) // some clients append a trailing "/"
	mux.HandleFunc("GET /api/config", s.handleShortConfig)
	mux.HandleFunc("GET /config", s.handleShortConfig)
	mux.HandleFunc("GET /api/{user}/config", s.handleConfig)
	mux.HandleFunc("GET /api/{user}", s.handleDatastore)
	mux.HandleFunc("GET /api/{user}/lights", s.handleLights)
	mux.HandleFunc("GET /api/{user}/lights/{id}", s.handleLight)
	mux.HandleFunc("PUT /api/{user}/lights/{id}/state", s.handleSetLightState)
	mux.HandleFunc("GET /api/{user}/groups", s.handleGroups)
	mux.HandleFunc("GET /api/{user}/groups/{id}", s.handleGroup)
	mux.HandleFunc("POST /api/{user}/groups", s.handleCreateGroup)
	mux.HandleFunc("POST /api/{user}/groups/", s.handleCreateGroup)
	mux.HandleFunc("PUT /api/{user}/groups/{id}/action", s.handleGroupAction)
	mux.HandleFunc("PUT /api/{user}/groups/{id}", s.handleGroupUpdate)
	mux.HandleFunc("GET /api/{user}/capabilities", s.handleCapabilities)
	mux.HandleFunc("GET /api/{user}/scenes", s.handleEmptyCollection)
	mux.HandleFunc("GET /api/{user}/schedules", s.handleEmptyCollection)
	mux.HandleFunc("GET /api/{user}/sensors", s.handleEmptyCollection)
	mux.HandleFunc("GET /api/{user}/rules", s.handleEmptyCollection)
	mux.HandleFunc("GET /api/{user}/resourcelinks", s.handleEmptyCollection)
	return s.logRequests(mux)
}

// logRequests logs every request. In debug mode it also logs the User-Agent and
// body — essential for analyzing the real behavior of unknown TVs
// (e.g. the devicetype string during pairing).
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		if s.Debug {
			var body []byte
			if r.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(r.Body, 4096))
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			s.log.Info("http rx",
				"method", r.Method,
				"path", r.URL.Path,
				"requestURI", r.URL.RequestURI(),
				"from", r.RemoteAddr,
				"host", r.Host,
				"user-agent", r.UserAgent(),
				"body", string(body),
			)
		} else if id, ok := lightStateWriteID(r); ok {
			// Ambilight pushes light-state writes many times per second over the
			// REST path; logging each one floods the log. Accumulate and let
			// LogActivitySummary emit a periodic rollup instead.
			s.recordLightWrite(id)
		} else if isGroupActionWrite(r) {
			// The group-action path is the other high-frequency control route;
			// accumulate it too so it does not flood and shows up in the Hz rollup.
			s.recordGroupActionWrite()
		} else {
			s.log.Info("http", "method", r.Method, "path", r.URL.Path, "from", r.RemoteAddr, "user-agent", r.UserAgent())
		}
		next.ServeHTTP(rec, r)
		if s.Debug {
			s.log.Info("http tx", "method", r.Method, "requestURI", r.URL.RequestURI(), "status", rec.status, "bytes", rec.bytes)
		}
	})
}

// lightStateWriteID returns the light ID of a PUT /api/{user}/lights/{id}/state
// request (the high-frequency Ambilight control write), and whether it matched.
func lightStateWriteID(r *http.Request) (string, bool) {
	if r.Method != http.MethodPut {
		return "", false
	}
	const lights = "/lights/"
	i := strings.Index(r.URL.Path, lights)
	if i < 0 || !strings.HasSuffix(r.URL.Path, "/state") {
		return "", false
	}
	id := r.URL.Path[i+len(lights) : len(r.URL.Path)-len("/state")]
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// isGroupActionWrite reports whether r is a PUT /api/{user}/groups/{id}/action —
// the group-based high-frequency control path (alternative to per-light writes).
func isGroupActionWrite(r *http.Request) bool {
	return r.Method == http.MethodPut &&
		strings.Contains(r.URL.Path, "/groups/") &&
		strings.HasSuffix(r.URL.Path, "/action")
}

// recordLightWrite accumulates one Ambilight light-state write for the periodic
// summary emitted by LogActivitySummary.
func (s *Server) recordLightWrite(id string) {
	s.activityMu.Lock()
	s.lightWrites++
	s.lightsTouched[id] = struct{}{}
	s.activityMu.Unlock()
}

// recordGroupActionWrite accumulates one group-action write for the summary.
func (s *Server) recordGroupActionWrite() {
	s.activityMu.Lock()
	s.groupActionWrites++
	s.activityMu.Unlock()
}

// MarkActivity stamps the most-recent-activity time from a non-REST source — the
// entertainment DTLS stream. In entertainment mode the TV streams frames over DTLS
// instead of REST writes, so without this the idle-off monitor (which watches
// LastActivity) would treat an actively-streaming TV as idle and flash the lights
// off mid-stream. The stream stopping then correctly lets idle-off fire.
func (s *Server) MarkActivity() {
	s.activityMu.Lock()
	s.lastWriteAt = time.Now()
	s.activityMu.Unlock()
}

// idleGapLogFloor is the smallest inter-write gap worth logging when gap tracing
// is on. It exists to calibrate the idle-off timeout: during a real viewing
// session the largest legitimate gap (static/dark/paused scenes) must stay well
// below the configured -idle-off-timeout.
const idleGapLogFloor = time.Second

// gapTrace gates the temporary inter-write gap log used to calibrate the
// idle-off timeout. It is a dedicated env var (not -debug) so a calibration run
// is not buried under per-request http rx/tx spam: set RELUME_GAP_TRACE=1 and
// grep "ambilight write gap". Remove once the timeout default is settled.
var gapTrace = os.Getenv("RELUME_GAP_TRACE") != ""

// recordWriteTime stamps the time of an Ambilight light-state write for the
// idle-off monitor (independent of Debug). With RELUME_GAP_TRACE set it also logs
// the gap since the previous write when it exceeds idleGapLogFloor, to calibrate
// the idle-off timeout against the TV's real maximum legitimate pause.
func (s *Server) recordWriteTime() {
	now := time.Now()
	s.activityMu.Lock()
	prev := s.lastWriteAt
	s.lastWriteAt = now
	s.activityMu.Unlock()
	if gapTrace && !prev.IsZero() {
		if gap := now.Sub(prev); gap >= idleGapLogFloor {
			s.log.Info("ambilight write gap", "gap", gap.Round(time.Millisecond).String())
		}
	}
}

// LastActivity returns the time of the most recent Ambilight light-state write
// (zero if none yet). Used by the idle-off monitor.
func (s *Server) LastActivity() time.Time {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	return s.lastWriteAt
}

// LogActivitySummary logs a rollup of the accumulated Ambilight light-state
// writes every interval (only when there was activity), then resets the counters.
// It blocks until ctx is cancelled; run it in a goroutine.
func (s *Server) LogActivitySummary(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.flushActivity(interval)
		}
	}
}

// flushActivity logs the accumulated Ambilight light-state writes (if any) and
// resets the counters. window is the period the counts cover.
func (s *Server) flushActivity(window time.Duration) {
	s.activityMu.Lock()
	writes, groupWrites, lights := s.lightWrites, s.groupActionWrites, len(s.lightsTouched)
	lastWrite := s.lastWriteAt
	s.lightWrites = 0
	s.groupActionWrites = 0
	s.lightsTouched = map[string]struct{}{}
	s.activityMu.Unlock()
	total := writes + groupWrites
	if total == 0 {
		return
	}
	// total_hz / per_light_hz turn the raw counts into the update rate — the
	// telltale for the lag: ~25 Hz means the TV streams fast (so the bottleneck is
	// forwarding to the Pro), ~1-2 Hz means it is stuck in the slow REST fallback.
	secs := window.Seconds()
	perLightHz := 0.0
	if lights > 0 && secs > 0 {
		perLightHz = float64(writes) / float64(lights) / secs
	}
	totalHz := 0.0
	if secs > 0 {
		totalHz = float64(total) / secs
	}

	attrs := []any{
		"light_state_writes", writes,
		"group_action_writes", groupWrites,
		"lights", lights,
		"window", window.String(),
		"total_hz", round1(totalHz),
		"per_light_hz", round1(perLightHz),
	}
	// Active (flash-target) lights the TV is currently driving — count and IDs.
	if s.ControlledLights != nil {
		ids := s.ControlledLights()
		attrs = append(attrs, "active_lights", len(ids), "active_light_ids", ids)
	}
	// Seconds since the last write — surfaces pauses approaching the idle-off.
	if !lastWrite.IsZero() {
		attrs = append(attrs, "since_last_write", time.Since(lastWrite).Round(time.Second).String())
	}
	// Forwarding health from the Bridge Pro provider (dropped/coalesced frames and
	// failed writes), if the backend exposes it.
	if ds, ok := s.lightProvider().(drainStatsProvider); ok {
		coalesced, forwardErrors := ds.DrainStatsDelta()
		attrs = append(attrs, "coalesced_frames", coalesced, "forward_errors", forwardErrors)
	}
	s.log.Info("ambilight activity", attrs...)
}

// drainStatsProvider is the optional backend capability to report per-window
// forwarding stats (coalesced frames and forward errors) for the activity rollup.
type drainStatsProvider interface {
	DrainStatsDelta() (coalesced, forwardErrors uint64)
}

// round1 rounds to one decimal for readable Hz figures in the activity log.
func round1(v float64) float64 { return math.Round(v*10) / 10 }

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

// isTVRequest identifies the Ambilight TV so pairing can be auto-accepted only
// for it (never an arbitrary LAN device): by source IP (when -tv-ip is set) or by
// the Android/Dalvik TV User-Agent it uses for CLIP v1 pairing
// (e.g. "Dalvik/2.1.0 (Linux; U; Android 11; 2021/22 Philips UHD Android TV ...)").
func (s *Server) isTVRequest(r *http.Request) bool {
	if s.TVIP != "" {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host == s.TVIP {
			return true
		}
	}
	ua := strings.ToLower(r.UserAgent())
	return strings.Contains(ua, "android") && (strings.Contains(ua, "philips") || strings.Contains(ua, "tv"))
}

func (s *Server) handleDescription(w http.ResponseWriter, r *http.Request) {
	relumeVariant := r.URL.Query().Get("relume")
	// relume=ms1 normally changes the descriptor body to MediaServer. The
	// MediaServerBasicBody experiment keeps that followed URL but serves Basic:1.
	// Other relume query variants keep the Hue Basic body and short cache headers.
	mediaServerAlias := s.MediaServerAlias && relumeVariant == "ms1" && !s.MediaServerBasicBody
	xml, err := upnp.RenderWithOptions(s.cfg.Identity, s.advIP, s.httpPort, upnp.Options{
		Profile:            s.IdentityProfile,
		DescriptionProfile: s.DescriptionProfile,
		MediaServerAlias:   mediaServerAlias,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Real Hue bridges and the confirmed-working ha-hue-entertainment emulator
	// serve description.xml as text/xml. application/xml is suspected to make the
	// Ambilight TV reject the descriptor and stop before POST /api.
	w.Header().Set("Content-Type", "text/xml")
	w.Header().Set("Server", upnp.ServerHeader(s.IdentityProfile))
	if relumeVariant != "" {
		w.Header().Set("Cache-Control", "max-age=1")
	} else {
		w.Header().Set("Cache-Control", "max-age=100")
	}
	io.WriteString(w, xml)
}

type pairingRequest struct {
	DeviceType        string `json:"devicetype"`
	GenerateClientKey bool   `json:"generateclientkey"`
}

func (s *Server) handlePairing(w http.ResponseWriter, r *http.Request) {
	var req pairingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 2, "/", "body contains invalid json")
		return
	}
	s.log.Info("pairing request", "devicetype", req.DeviceType, "clientkey", req.GenerateClientKey, "from", r.RemoteAddr)

	// Pairing is auto-accepted, but only for the TV — never an arbitrary LAN
	// device. Non-TV requests get the standard CLIP v1 error 101.
	if !s.isTVRequest(r) {
		writeError(w, 101, "", "link button not pressed")
		return
	}

	// Idempotent: the TV polls POST /api rapidly; return the existing credentials
	// for a devicetype instead of minting (and persisting) a new user each time.
	if existing, ok := s.cfg.ApiUserByDeviceType(req.DeviceType); ok {
		success := map[string]string{"username": existing.Username}
		if existing.ClientKey != "" {
			success["clientkey"] = existing.ClientKey
		}
		writeJSON(w, []map[string]any{{"success": success}})
		return
	}

	// Hold off the first auto-pairing for pairAcceptDelay — purely so it feels
	// natural, like pairing with a real bridge after a link-button tap (relume
	// auto-accepts regardless; this is cosmetic, not a requirement). The TV polls
	// POST /api and keeps trying (it waits up to ~30s), so returning the standard
	// 101 until the window elapses just delays acceptance without aborting the TV.
	s.pairMu.Lock()
	if s.firstPairSeen.IsZero() {
		s.firstPairSeen = time.Now()
	}
	waited := time.Since(s.firstPairSeen)
	s.pairMu.Unlock()
	if waited < s.pairAcceptDelay {
		writeError(w, 101, "", "link button not pressed")
		return
	}

	username, err := randomHex(16) // 32 characters
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	user := &config.ApiUser{Username: username, DeviceType: req.DeviceType}

	success := map[string]string{"username": username}
	if req.GenerateClientKey {
		ck, cerr := randomHex(16)
		if cerr != nil {
			http.Error(w, cerr.Error(), http.StatusInternalServerError)
			return
		}
		ck = strings.ToUpper(ck)
		user.ClientKey = ck
		success["clientkey"] = ck
	}
	if err := s.cfg.AddApiUser(user); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("tv paired", "username", username, "entertainment", req.GenerateClientKey)

	writeJSON(w, []map[string]any{{"success": success}})
}

// handleShortConfig returns the unauthenticated short config (identity check).
func (s *Server) handleShortConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.shortConfig())
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if s.IdentityProfile == "ambilight" && !s.cfg.HasApiUser(r.PathValue("user")) {
		writeJSON(w, s.shortConfig())
		return
	}
	if !s.authorized(w, r) {
		return
	}
	writeJSON(w, s.shortConfig())
}

// shortConfig builds the config object; modelid MUST be BSB002.
func (s *Server) shortConfig() map[string]any {
	id := s.cfg.Identity
	datastoreVersion := "131"
	if s.IdentityProfile == "ambilight" {
		datastoreVersion = "126"
	}
	return map[string]any{
		"name":             "Relume",
		"datastoreversion": datastoreVersion,
		"swversion":        "1967054020",
		"apiversion":       "1.67.0",
		"mac":              id.MAC(),
		"bridgeid":         id.BridgeID(),
		"factorynew":       false,
		"replacesbridgeid": nil,
		"modelid":          "BSB002",
		"starterkitid":     "",
	}
}

// handleDatastore returns the top-level structure that some clients query after
// pairing.
func (s *Server) handleDatastore(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	writeJSON(w, map[string]any{
		"lights":        s.lightsV1(),
		"groups":        map[string]any{},
		"config":        s.shortConfig(),
		"schedules":     map[string]any{},
		"scenes":        map[string]any{},
		"rules":         map[string]any{},
		"sensors":       map[string]any{},
		"resourcelinks": map[string]any{},
	})
}

// handleLights returns the lights of the Bridge Pro (v1-translated) or an empty
// list if no backend is paired yet (M1).
func (s *Server) handleLights(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	writeJSON(w, s.lightsV1())
}

func (s *Server) handleLight(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id := r.PathValue("id")
	lp := s.lightProvider()
	if lp == nil {
		writeError(w, 3, "/lights/"+id, "resource, /lights/"+id+", not available")
		return
	}
	lights, err := lp.LightsV1()
	if err != nil {
		s.log.Warn("reading lights from bridge pro", "err", err)
		writeError(w, 901, "/lights/"+id, "bridge pro error")
		return
	}
	light, ok := lights[id]
	if !ok {
		writeError(w, 3, "/lights/"+id, "resource, /lights/"+id+", not available")
		return
	}
	writeJSON(w, light)
}

// handleSetLightState handles the REST control path: accept v1 state, translate
// it to v2 and forward it to the Bridge Pro.
func (s *Server) handleSetLightState(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id := r.PathValue("id")
	var state map[string]any
	if err := json.NewDecoder(r.Body).Decode(&state); err != nil {
		writeError(w, 2, "/lights/"+id+"/state", "invalid json")
		return
	}
	lp := s.lightProvider()
	if lp == nil {
		writeError(w, 3, "/lights/"+id, "no bridge pro paired")
		return
	}
	s.recordWriteTime()
	// Optimistic: the provider queues the write and forwards it to the Bridge Pro
	// asynchronously, so this returns immediately without blocking on the round-trip.
	if err := lp.SetLightV1(id, state); err != nil {
		s.log.Warn("setting light", "id", id, "err", err)
		writeError(w, 901, "/lights/"+id+"/state", "bridge pro error")
		return
	}
	// v1 success response: one success entry per field that was set.
	resp := make([]map[string]any, 0, len(state))
	for k, v := range state {
		resp = append(resp, map[string]any{"success": map[string]any{
			"/lights/" + id + "/state/" + k: v,
		}})
	}
	writeJSON(w, resp)
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	writeJSON(w, map[string]any{
		"0": s.bridgeGroup("0"),
		"1": s.bridgeGroup("1"),
	})
}

func (s *Server) bridgeGroup(id string) map[string]any {
	groupType := "Entertainment"
	name := "Relume Entertainment"
	if id == "0" {
		groupType = "LightGroup"
		name = "Group 0"
	}
	// Default: inactive stream. Under the entertainment probe, reflect the
	// activation the TV requested so it treats the stream as live and proceeds to
	// open the DTLS connection (which the :2100 probe then observes).
	var streamActive bool
	var streamOwner any
	if s.confirmsEntertainment() && id == "1" {
		s.streamMu.Lock()
		streamActive = s.streamActive
		if s.streamOwner != "" {
			streamOwner = s.streamOwner
		}
		s.streamMu.Unlock()
	}
	return map[string]any{
		"name":   name,
		"lights": []string{},
		"type":   groupType,
		"state":  map[string]any{"all_on": false, "any_on": false},
		"action": map[string]any{},
		"stream": map[string]any{
			"active":    streamActive,
			"owner":     streamOwner,
			"proxymode": "auto",
			"proxynode": "/bridge",
		},
	}
}

func (s *Server) handleGroup(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id := r.PathValue("id")
	if id != "0" && id != "1" {
		writeError(w, 3, "/groups/"+id, "resource, /groups/"+id+", not available")
		return
	}
	writeJSON(w, s.bridgeGroup(id))
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	body, _ := io.ReadAll(r.Body)
	if s.confirmsEntertainment() {
		var g struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &g)
		s.log.Info("ENTERTAINMENT group create requested by TV",
			"type", g.Type, "name", g.Name, "body", string(body))
	} else {
		s.log.Info("group create (not yet persisted)", "body", string(body))
	}
	writeJSON(w, []map[string]any{{"success": map[string]any{"id": "1"}}})
}

// handleGroupAction is the groups REST path. Full group/entertainment support
// follows in M4; for now the request is logged and acknowledged so that the TV
// does not abort.
func (s *Server) handleGroupAction(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id := r.PathValue("id")
	body, _ := io.ReadAll(r.Body)
	s.log.Info("group action (not yet forwarded)", "group", id, "body", string(body))
	writeJSON(w, []map[string]any{{"success": map[string]any{"/groups/" + id + "/action": "ok"}}})
}

// handleGroupUpdate intercepts, among other things, the stream activation
// (PUT /groups/{id} with {"stream":{"active":true}}) — the entry into the
// entertainment path (M4).
//
// Under the entertainment probe, a stream-activation PUT is confirmed with the
// real v1 success shape ([{"success":{"/groups/1/stream/active":true}}]) and the
// group's stream state is updated, so the TV treats activation as accepted and
// goes on to open the DTLS stream (which the :2100 probe observes). Without the
// probe the legacy log-and-ack behavior is kept.
func (s *Server) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id := r.PathValue("id")
	body, _ := io.ReadAll(r.Body)
	s.log.Info("group update", "group", id, "body", string(body))

	if s.confirmsEntertainment() {
		if active, ok := streamActiveFromBody(body); ok {
			owner := r.PathValue("user")
			s.streamMu.Lock()
			s.streamActive = active
			if active {
				s.streamOwner = owner
			} else {
				s.streamOwner = ""
			}
			s.streamMu.Unlock()
			s.log.Info("ENTERTAINMENT stream activation requested by TV",
				"group", id, "active", active, "owner", owner)
			writeJSON(w, []map[string]any{{"success": map[string]any{
				"/groups/" + id + "/stream/active": active,
			}}})
			return
		}
	}
	writeJSON(w, []map[string]any{{"success": map[string]any{"/groups/" + id: "ok"}}})
}

// streamActiveFromBody extracts stream.active from a group-update body. The bool
// return is the requested value; ok is false when the body carries no stream
// field (an ordinary group update, not a stream activation).
func streamActiveFromBody(body []byte) (active, ok bool) {
	var req struct {
		Stream *struct {
			Active *bool `json:"active"`
		} `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Stream == nil || req.Stream.Active == nil {
		return false, false
	}
	return *req.Stream.Active, true
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	writeJSON(w, map[string]any{
		"lights":        map[string]any{"available": 60, "total": 63},
		"sensors":       map[string]any{"available": 240, "total": 250},
		"groups":        map[string]any{"available": 60, "total": 64},
		"scenes":        map[string]any{"available": 172, "total": 200},
		"rules":         map[string]any{"available": 233, "total": 250},
		"schedules":     map[string]any{"available": 95, "total": 100},
		"resourcelinks": map[string]any{"available": 59, "total": 64},
		"streaming":     map[string]any{"available": 1, "total": 1, "channels": 20},
		"timezones":     map[string]any{"values": []string{"Etc/UTC"}},
	})
}

func (s *Server) handleEmptyCollection(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	writeJSON(w, map[string]any{})
}

// authorized checks whether the {user} from the path is a paired client.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	user := r.PathValue("user")
	if !s.cfg.HasApiUser(user) {
		writeError(w, 1, "/"+strings.TrimPrefix(r.URL.Path, "/api/"), "unauthorized user")
		return false
	}
	return true
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a CLIP-v1 error in the standard format.
func writeError(w http.ResponseWriter, typ int, address, description string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode([]map[string]any{{
		"error": map[string]any{"type": typ, "address": address, "description": description},
	}})
}
