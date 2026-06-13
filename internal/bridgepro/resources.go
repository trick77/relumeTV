package bridgepro

import (
	"sort"
)

// Light ist die für relume relevante Teilmenge einer CLIP-v2-light-Ressource.
type Light struct {
	ID       string `json:"id"`
	IDv1     string `json:"id_v1"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	On struct {
		On bool `json:"on"`
	} `json:"on"`
	Dimming struct {
		Brightness float64 `json:"brightness"`
	} `json:"dimming"`
	Color struct {
		XY struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
		} `json:"xy"`
	} `json:"color"`
	ColorTemperature struct {
		Mirek int `json:"mirek"`
	} `json:"color_temperature"`
	// Owner verweist auf das zugehörige device (für stabile Sortierung/Namen).
	Owner struct {
		RID string `json:"rid"`
	} `json:"owner"`
}

type lightList struct {
	Errors []any   `json:"errors"`
	Data   []Light `json:"data"`
}

// Lights liest alle Lampen der Bridge Pro, stabil nach ID sortiert.
func (c *Client) Lights() ([]Light, error) {
	var ll lightList
	if err := c.get("/clip/v2/resource/light", &ll); err != nil {
		return nil, err
	}
	sort.Slice(ll.Data, func(i, j int) bool { return ll.Data[i].ID < ll.Data[j].ID })
	return ll.Data, nil
}

// EntertainmentConfig ist die für relume relevante Teilmenge einer
// entertainment_configuration-Ressource.
type EntertainmentConfig struct {
	ID       string `json:"id"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status string `json:"status"` // "inactive" | "active"
}

type entConfigList struct {
	Errors []any                 `json:"errors"`
	Data   []EntertainmentConfig `json:"data"`
}

// EntertainmentConfigs liest die Entertainment-Konfigurationen der Bridge Pro.
func (c *Client) EntertainmentConfigs() ([]EntertainmentConfig, error) {
	var el entConfigList
	if err := c.get("/clip/v2/resource/entertainment_configuration", &el); err != nil {
		return nil, err
	}
	sort.Slice(el.Data, func(i, j int) bool { return el.Data[i].ID < el.Data[j].ID })
	return el.Data, nil
}
