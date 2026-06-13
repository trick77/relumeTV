// Package clipv1 provides the CLIP-v1 HTTP interface that the Ambilight TV
// expects: /description.xml, pairing (POST /api), config and (in later
// milestones) lights/groups as well as activating the entertainment stream.
package clipv1

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/trick77/relume/internal/config"
	"github.com/trick77/relume/internal/upnp"
)

// linkWindow is the duration during which a pairing is accepted after pressing
// the (virtual) link button — just like on a real bridge.
const linkWindow = 30 * time.Second

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
	lights   LightProvider
	// Debug enables verbose request logging (User-Agent + body) — helpful for
	// analyzing the real behavior of unknown TVs.
	Debug bool
	// IdentityProfile selects experimental wire-identity compatibility tweaks.
	// Empty keeps the default; "ambilight" matches the Ambilight-specific
	// OSS emulator; "hass" matches Home Assistant emulated-hue.
	IdentityProfile string
	// MediaServerAlias makes /description.xml match the opt-in SSDP MediaServer:1 alias.
	MediaServerAlias bool

	mu       sync.Mutex
	lastLink time.Time
}

// New creates the CLIP-v1 server.
func New(cfg *config.Config, advIP string, httpPort int, log *slog.Logger) *Server {
	return &Server{cfg: cfg, advIP: advIP, httpPort: httpPort, log: log}
}

// SetLightProvider registers the source for the light list (Bridge Pro backend).
func (s *Server) SetLightProvider(p LightProvider) {
	s.lights = p
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
	// Virtual link button (web UI / CLI open the pairing window).
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("POST /link", s.handleLink)
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
		} else {
			s.log.Info("http", "method", r.Method, "path", r.URL.Path, "from", r.RemoteAddr)
		}
		next.ServeHTTP(rec, r)
		if s.Debug {
			s.log.Info("http tx", "method", r.Method, "requestURI", r.URL.RequestURI(), "status", rec.status, "bytes", rec.bytes)
		}
	})
}

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

// PressLink opens the pairing window (used by the CLI `link` command or the web UI).
func (s *Server) PressLink() {
	s.mu.Lock()
	s.lastLink = time.Now()
	s.mu.Unlock()
	s.log.Info("link button pressed", "fenster", linkWindow)
}

func (s *Server) linkActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastLink) <= linkWindow
}

func (s *Server) handleDescription(w http.ResponseWriter, r *http.Request) {
	relumeVariant := r.URL.Query().Get("relume")
	// relume=ms1 is the only variant that changes the descriptor body to MediaServer.
	// Other relume query variants keep the Hue Basic body but use short cache headers.
	mediaServerAlias := s.MediaServerAlias && relumeVariant == "ms1"
	xml, err := upnp.RenderWithOptions(s.cfg.Identity, s.advIP, s.httpPort, upnp.Options{
		Profile:          s.IdentityProfile,
		MediaServerAlias: mediaServerAlias,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
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
	s.log.Info("pairing request", "devicetype", req.DeviceType, "clientkey", req.GenerateClientKey)

	if !s.linkActive() {
		// CLIP-v1 standard error 101: link button not pressed.
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
		"name":             "Philips hue",
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
		"lights":        map[string]any{},
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
	if s.lights == nil {
		writeJSON(w, map[string]any{})
		return
	}
	lights, err := s.lights.LightsV1()
	if err != nil {
		s.log.Warn("reading lights from bridge pro", "err", err)
		writeJSON(w, map[string]any{})
		return
	}
	writeJSON(w, lights)
}

func (s *Server) handleLight(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id := r.PathValue("id")
	if s.lights == nil {
		writeError(w, 3, "/lights/"+id, "resource, /lights/"+id+", not available")
		return
	}
	lights, err := s.lights.LightsV1()
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
	if s.lights == nil {
		writeError(w, 3, "/lights/"+id, "no bridge pro paired")
		return
	}
	if err := s.lights.SetLightV1(id, state); err != nil {
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
		"0": bridgeGroup("0"),
		"1": bridgeGroup("1"),
	})
}

func bridgeGroup(id string) map[string]any {
	groupType := "Entertainment"
	name := "Relume Entertainment"
	if id == "0" {
		groupType = "LightGroup"
		name = "Group 0"
	}
	return map[string]any{
		"name":   name,
		"lights": []string{},
		"type":   groupType,
		"state":  map[string]any{"all_on": false, "any_on": false},
		"action": map[string]any{},
		"stream": map[string]any{
			"active":    false,
			"owner":     nil,
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
	writeJSON(w, bridgeGroup(id))
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	body, _ := io.ReadAll(r.Body)
	s.log.Info("group create (not yet persisted)", "body", string(body))
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
func (s *Server) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id := r.PathValue("id")
	body, _ := io.ReadAll(r.Body)
	s.log.Info("group update", "group", id, "body", string(body))
	writeJSON(w, []map[string]any{{"success": map[string]any{"/groups/" + id: "ok"}}})
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

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, indexHTML)
}

func (s *Server) handleLink(w http.ResponseWriter, _ *http.Request) {
	s.PressLink()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, fmt.Sprintf("<p>Link button pressed. Pairing open for %s.</p><p><a href=\"/\">back</a></p>", linkWindow))
}

const indexHTML = `<!doctype html><html><head><meta charset="utf-8"><title>relume</title></head>
<body style="font-family:sans-serif;max-width:40em;margin:2em auto">
<h1>relume</h1>
<p>Software bridge for Philips Ambilight TV &harr; Hue Bridge Pro.</p>
<form method="post" action="/link"><button type="submit">Press link button (open pairing)</button></form>
</body></html>`

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
