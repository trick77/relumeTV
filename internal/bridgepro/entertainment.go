package bridgepro

import "fmt"

// EntertainmentService is the subset of a CLIP v2 entertainment resource relevant
// for relume. Every color-capable light's owning device exposes one; relume maps a
// light to its entertainment service via the shared owner device rid, then uses the
// service rid as the member of an entertainment_configuration channel.
type EntertainmentService struct {
	ID    string `json:"id"`
	Owner struct {
		RID   string `json:"rid"`
		RType string `json:"rtype"`
	} `json:"owner"`
	Renderer bool `json:"renderer"`
}

type entServiceList struct {
	Errors []any                  `json:"errors"`
	Data   []EntertainmentService `json:"data"`
}

// EntertainmentServices reads the entertainment services of the Hue Bridge Pro. Each
// carries its owning device rid (matching a light's owner.rid), used to build the
// channel→light mapping for the entertainment_configuration.
func (c *Client) EntertainmentServices() ([]EntertainmentService, error) {
	var el entServiceList
	if err := c.get("/clip/v2/resource/entertainment", &el); err != nil {
		return nil, err
	}
	return el.Data, nil
}

// EntChannel is one channel of an entertainment_configuration as the Hue Bridge Pro
// returns it. channel_id is assigned by the bridge (do NOT assume 0..N-1) and its
// members reference the entertainment service(s) it drives.
type EntChannel struct {
	ChannelID int `json:"channel_id"`
	Members   []struct {
		Service struct {
			RID   string `json:"rid"`
			RType string `json:"rtype"`
		} `json:"service"`
		Index int `json:"index"`
	} `json:"members"`
}

// EntertainmentConfigFull is the full entertainment_configuration resource relume
// reads back after creating it, to learn the bridge-assigned channel ids.
type EntertainmentConfigFull struct {
	ID       string `json:"id"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status   string       `json:"status"`
	Channels []EntChannel `json:"channels"`
}

type entConfigFullList struct {
	Errors []any                     `json:"errors"`
	Data   []EntertainmentConfigFull `json:"data"`
}

// GetEntertainmentConfig reads one entertainment_configuration by id, including the
// bridge-assigned channels (used to map TV light ids → Pro channel ids).
func (c *Client) GetEntertainmentConfig(id string) (*EntertainmentConfigFull, error) {
	var el entConfigFullList
	if err := c.get("/clip/v2/resource/entertainment_configuration/"+id, &el); err != nil {
		return nil, err
	}
	if len(el.Data) == 0 {
		return nil, fmt.Errorf("entertainment_configuration %s not found", id)
	}
	return &el.Data[0], nil
}

// ConfigMember is one light's entertainment service plus a (cosmetic) position to
// place in a new entertainment_configuration. Pass-through streaming ignores the
// position, so trivial values are fine.
type ConfigMember struct {
	ServiceRID string
	X, Y, Z    float64
}

// CreateEntertainmentConfig creates a screen-type entertainment_configuration named
// `name` whose service_locations cover the given members, and returns the new
// configuration id. The bridge generates the channels from the locations; read them
// back with GetEntertainmentConfig to learn the assigned channel ids.
//
// NOTE: the exact payload accepted by the BSB003 is validated against the real Pro
// during rollout — keep this builder isolated so its shape can be adjusted without
// touching the streamer (see the Phase C design's Open items).
func (c *Client) CreateEntertainmentConfig(name string, members []ConfigMember) (string, error) {
	locations := make([]map[string]any, 0, len(members))
	for _, m := range members {
		locations = append(locations, map[string]any{
			"service": map[string]any{"rid": m.ServiceRID, "rtype": "entertainment"},
			"positions": []map[string]any{
				{"x": m.X, "y": m.Y, "z": m.Z},
			},
			"equalization_factor": 1.0,
		})
	}
	payload := map[string]any{
		"type":               "entertainment_configuration",
		"metadata":           map[string]any{"name": name},
		"configuration_type": "screen",
		"stream_proxy":       map[string]any{"mode": "auto"},
		"locations":          map[string]any{"service_locations": locations},
	}
	return c.post("/clip/v2/resource/entertainment_configuration", payload)
}

// StartStream activates the entertainment stream for the configuration (PUT
// {"action":"start"}); the bridge then accepts the DTLS stream on udp :2100.
func (c *Client) StartStream(id string) error {
	return c.put("/clip/v2/resource/entertainment_configuration/"+id, map[string]any{"action": "start"})
}

// StopStream deactivates the entertainment stream (PUT {"action":"stop"}). Always
// called on teardown so the area does not stay active and block other apps.
func (c *Client) StopStream(id string) error {
	return c.put("/clip/v2/resource/entertainment_configuration/"+id, map[string]any{"action": "stop"})
}

// DeleteEntertainmentConfig removes an entertainment_configuration by id. relume
// uses it to drop its own `relume` config when the color-light set changed under it
// (so a stale config does not linger or count against the Pro's area limit). Stop
// the stream first if it might be active — the Pro rejects deleting an active one.
func (c *Client) DeleteEntertainmentConfig(id string) error {
	return c.del("/clip/v2/resource/entertainment_configuration/" + id)
}
