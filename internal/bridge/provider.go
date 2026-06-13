// Package bridge verdrahtet das TV-seitige Frontend (clipv1) mit dem
// Bridge-Pro-seitigen Backend (bridgepro) und hält das Lampen-Mapping.
package bridge

import (
	"fmt"
	"sync"
	"time"

	"github.com/trick77/relume/internal/bridgepro"
	"github.com/trick77/relume/internal/translate"
)

// lightCacheTTL begrenzt, wie oft die Bridge Pro nach Lampen gefragt wird.
const lightCacheTTL = 5 * time.Second

// LightProvider implementiert clipv1.LightProvider auf Basis der Bridge Pro und
// hält das v1→UUID-Mapping für die spätere Steuerung (M3).
type LightProvider struct {
	client *bridgepro.Client

	mu        sync.Mutex
	cached    map[string]any
	v1ToUUID  map[string]string
	fetchedAt time.Time
}

// NewLightProvider erstellt einen Provider für die gegebene Bridge Pro.
func NewLightProvider(client *bridgepro.Client) *LightProvider {
	return &LightProvider{client: client}
}

// LightsV1 liefert die v1-Lampenliste (mit kurzem Cache).
func (p *LightProvider) LightsV1() (map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached != nil && time.Since(p.fetchedAt) < lightCacheTTL {
		return p.cached, nil
	}
	lights, err := p.client.Lights()
	if err != nil {
		return nil, err
	}
	lm := translate.LightsV1(lights)
	p.cached = lm.V1
	p.v1ToUUID = lm.V1ToUUID
	p.fetchedAt = time.Now()
	return p.cached, nil
}

// UUIDForV1 liefert die v2-UUID zu einer numerischen v1-Light-ID.
func (p *LightProvider) UUIDForV1(v1id string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	uuid, ok := p.v1ToUUID[v1id]
	return uuid, ok
}

// SetLightV1 setzt den Zustand einer Lampe anhand ihrer v1-ID. Der v1-State wird
// nach v2 übersetzt und an die Bridge Pro weitergereicht (REST-Fallback-Pfad).
func (p *LightProvider) SetLightV1(v1id string, v1state map[string]any) error {
	uuid, ok := p.UUIDForV1(v1id)
	if !ok {
		// Mapping ggf. noch nicht aufgebaut → einmal Lampen laden und erneut prüfen.
		if _, err := p.LightsV1(); err != nil {
			return err
		}
		if uuid, ok = p.UUIDForV1(v1id); !ok {
			return fmt.Errorf("unbekannte light-id %q", v1id)
		}
	}
	return p.client.SetLight(uuid, translate.StateV1ToV2(v1state))
}
