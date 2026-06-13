// Package clipv1 stellt die CLIP-v1-HTTP-Oberfläche bereit, die der Ambilight-TV
// erwartet: /description.xml, Pairing (POST /api), Config und (in späteren
// Meilensteinen) Lampen/Gruppen sowie das Aktivieren des Entertainment-Streams.
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

// linkWindow ist die Dauer, in der nach Drücken des (virtuellen) Link-Buttons ein
// Pairing akzeptiert wird — wie an einer echten Bridge.
const linkWindow = 30 * time.Second

// LightProvider liefert die (bereits nach v1 übersetzte) Lampenliste der Bridge Pro
// und setzt Lampenzustände (REST-Fallback). Wird vom Backend (M2+) gesetzt; ist es
// nil, liefert der Server leere Listen (M1).
type LightProvider interface {
	// LightsV1 liefert die v1-Lampenliste (key = numerische ID als String).
	LightsV1() (map[string]any, error)
	// SetLightV1 setzt den Zustand einer Lampe anhand ihrer v1-ID.
	SetLightV1(v1id string, v1state map[string]any) error
}

// Server bedient die CLIP-v1-Oberfläche.
type Server struct {
	cfg      *config.Config
	advIP    string
	httpPort int
	log      *slog.Logger
	lights   LightProvider
	// Debug aktiviert ausführliches Request-Logging (User-Agent + Body) — hilfreich,
	// um das reale Verhalten unbekannter TVs zu analysieren.
	Debug bool

	mu       sync.Mutex
	lastLink time.Time
}

// New erstellt den CLIP-v1-Server.
func New(cfg *config.Config, advIP string, httpPort int, log *slog.Logger) *Server {
	return &Server{cfg: cfg, advIP: advIP, httpPort: httpPort, log: log}
}

// SetLightProvider hinterlegt die Quelle für die Lampenliste (Bridge-Pro-Backend).
func (s *Server) SetLightProvider(p LightProvider) {
	s.lights = p
}

// Handler liefert den HTTP-Handler (Routing) für den Server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /description.xml", s.handleDescription)
	mux.HandleFunc("POST /api", s.handlePairing)
	mux.HandleFunc("POST /api/", s.handlePairing) // manche Clients hängen ein "/" an
	mux.HandleFunc("GET /api/config", s.handleShortConfig)
	mux.HandleFunc("GET /config", s.handleShortConfig)
	mux.HandleFunc("GET /api/{user}/config", s.handleConfig)
	mux.HandleFunc("GET /api/{user}", s.handleDatastore)
	mux.HandleFunc("GET /api/{user}/lights", s.handleLights)
	mux.HandleFunc("PUT /api/{user}/lights/{id}/state", s.handleSetLightState)
	mux.HandleFunc("GET /api/{user}/groups", s.handleGroups)
	mux.HandleFunc("PUT /api/{user}/groups/{id}/action", s.handleGroupAction)
	mux.HandleFunc("PUT /api/{user}/groups/{id}", s.handleGroupUpdate)
	// Virtueller Link-Button (Web-UI / CLI öffnen das Pairing-Fenster).
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("POST /link", s.handleLink)
	return s.logRequests(mux)
}

// logRequests protokolliert jede Anfrage. Im Debug-Modus zusätzlich User-Agent und
// Body — entscheidend, um das reale Verhalten unbekannter TVs zu analysieren
// (z.B. den devicetype-String beim Pairing).
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Debug {
			var body []byte
			if r.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(r.Body, 4096))
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			s.log.Info("http rx",
				"method", r.Method,
				"path", r.URL.Path,
				"from", r.RemoteAddr,
				"user-agent", r.UserAgent(),
				"body", string(body),
			)
		} else {
			s.log.Info("http", "method", r.Method, "path", r.URL.Path, "from", r.RemoteAddr)
		}
		next.ServeHTTP(w, r)
	})
}

// PressLink öffnet das Pairing-Fenster (vom CLI-Befehl `link` oder der Web-UI genutzt).
func (s *Server) PressLink() {
	s.mu.Lock()
	s.lastLink = time.Now()
	s.mu.Unlock()
	s.log.Info("link-button gedrückt", "fenster", linkWindow)
}

func (s *Server) linkActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastLink) <= linkWindow
}

func (s *Server) handleDescription(w http.ResponseWriter, _ *http.Request) {
	xml, err := upnp.Render(s.cfg.Identity, s.advIP, s.httpPort)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
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
	s.log.Info("pairing-anfrage", "devicetype", req.DeviceType, "clientkey", req.GenerateClientKey)

	if !s.linkActive() {
		// CLIP-v1-Standardfehler 101: link button not pressed.
		writeError(w, 101, "", "link button not pressed")
		return
	}

	username, err := randomHex(16) // 32 Zeichen
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
	s.log.Info("tv gekoppelt", "username", username, "entertainment", req.GenerateClientKey)

	writeJSON(w, []map[string]any{{"success": success}})
}

// handleShortConfig liefert die unauthentifizierte Kurz-Config (Identitätscheck).
func (s *Server) handleShortConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.shortConfig())
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	writeJSON(w, s.shortConfig())
}

// shortConfig baut das Config-Objekt; modelid MUSS BSB002 sein.
func (s *Server) shortConfig() map[string]any {
	id := s.cfg.Identity
	return map[string]any{
		"name":             "Philips hue",
		"datastoreversion": "131",
		"swversion":        "1967054020",
		"apiversion":       "1.67.0",
		"mac":              id.MAC(),
		"bridgeid":         id.BridgeID(),
		"factorynew":       false,
		"replacesbridgeid":  nil,
		"modelid":          "BSB002",
		"starterkitid":     "",
	}
}

// handleDatastore liefert die Top-Level-Struktur, die einige Clients nach dem
// Pairing abfragen.
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

// handleLights liefert die Lampen der Bridge Pro (v1-übersetzt) oder eine leere
// Liste, wenn noch kein Backend gekoppelt ist (M1).
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
		s.log.Warn("lampen von bridge pro lesen", "err", err)
		writeJSON(w, map[string]any{})
		return
	}
	writeJSON(w, lights)
}

// handleSetLightState verarbeitet den REST-Steuerungspfad: v1-State entgegennehmen,
// nach v2 übersetzen und an die Bridge Pro weiterreichen.
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
		s.log.Warn("lampe setzen", "id", id, "err", err)
		writeError(w, 901, "/lights/"+id+"/state", "bridge pro error")
		return
	}
	// v1-Erfolgsantwort: ein success-Eintrag pro gesetztem Feld.
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
	writeJSON(w, map[string]any{})
}

// handleGroupAction ist der Gruppen-REST-Pfad. Vollständige Gruppen-/Entertainment-
// Unterstützung folgt in M4; vorerst wird die Anfrage geloggt und bestätigt, damit
// der TV nicht abbricht.
func (s *Server) handleGroupAction(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id := r.PathValue("id")
	body, _ := io.ReadAll(r.Body)
	s.log.Info("group action (noch nicht weitergereicht)", "group", id, "body", string(body))
	writeJSON(w, []map[string]any{{"success": map[string]any{"/groups/" + id + "/action": "ok"}}})
}

// handleGroupUpdate fängt u.a. die Stream-Aktivierung ab (PUT /groups/{id} mit
// {"stream":{"active":true}}) — der Einstieg in den Entertainment-Pfad (M4).
func (s *Server) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	id := r.PathValue("id")
	body, _ := io.ReadAll(r.Body)
	s.log.Info("group update", "group", id, "body", string(body))
	writeJSON(w, []map[string]any{{"success": map[string]any{"/groups/" + id: "ok"}}})
}

// authorized prüft, ob der {user} aus dem Pfad ein gekoppelter Client ist.
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
	io.WriteString(w, fmt.Sprintf("<p>Link-Button gedrückt. Pairing für %s offen.</p><p><a href=\"/\">zurück</a></p>", linkWindow))
}

const indexHTML = `<!doctype html><html><head><meta charset="utf-8"><title>relume</title></head>
<body style="font-family:sans-serif;max-width:40em;margin:2em auto">
<h1>relume</h1>
<p>Software-Bridge für Philips Ambilight-TV &harr; Hue Bridge Pro.</p>
<form method="post" action="/link"><button type="submit">Link-Button drücken (Pairing öffnen)</button></form>
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

// writeError schreibt einen CLIP-v1-Fehler im Standardformat.
func writeError(w http.ResponseWriter, typ int, address, description string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode([]map[string]any{{
		"error": map[string]any{"type": typ, "address": address, "description": description},
	}})
}
