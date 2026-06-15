package bridgepro

import (
	"fmt"
	"sort"
)

// bridgeResource is the subset of a CLIP v2 bridge resource relevant for relume.
type bridgeResource struct {
	ID       string `json:"id"`
	BridgeID string `json:"bridge_id"`
	Owner    struct {
		RID string `json:"rid"`
	} `json:"owner"`
}

// deviceResource is the subset of a CLIP v2 device resource relevant for relume
// (used to read the bridge's user-set name via the bridge's owning device).
type deviceResource struct {
	ID       string `json:"id"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

// BridgeInfo returns the Bridge Pro's user-set name and bridge id (best-effort:
// either may be empty if the bridge does not report it). The name comes from the
// device that owns the bridge resource. Requires the Pro to be reachable.
func (c *Client) BridgeInfo() (name, bridgeID string, err error) {
	var bl struct {
		Data []bridgeResource `json:"data"`
	}
	if err := c.get("/clip/v2/resource/bridge", &bl); err != nil {
		return "", "", err
	}
	if len(bl.Data) == 0 {
		return "", "", fmt.Errorf("no bridge resource returned")
	}
	b := bl.Data[0]
	bridgeID = b.BridgeID
	if b.Owner.RID == "" {
		return "", bridgeID, nil
	}
	var dl struct {
		Data []deviceResource `json:"data"`
	}
	if err := c.get("/clip/v2/resource/device/"+b.Owner.RID, &dl); err != nil {
		return "", bridgeID, nil // name is best-effort; keep the id
	}
	if len(dl.Data) > 0 {
		name = dl.Data[0].Metadata.Name
	}
	return name, bridgeID, nil
}

// Light is the subset of a CLIP v2 light resource that is relevant for relume.
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
	// Owner references the associated device (for stable sorting/names).
	Owner struct {
		RID string `json:"rid"`
	} `json:"owner"`
}

type lightList struct {
	Errors []any   `json:"errors"`
	Data   []Light `json:"data"`
}

// Lights reads all lights of the Bridge Pro, stably sorted by ID.
func (c *Client) Lights() ([]Light, error) {
	var ll lightList
	if err := c.get("/clip/v2/resource/light", &ll); err != nil {
		return nil, err
	}
	sort.Slice(ll.Data, func(i, j int) bool { return ll.Data[i].ID < ll.Data[j].ID })
	return ll.Data, nil
}

// EntertainmentConfig is the subset of an entertainment_configuration resource
// that is relevant for relume.
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

// EntertainmentConfigs reads the entertainment configurations of the Bridge Pro.
func (c *Client) EntertainmentConfigs() ([]EntertainmentConfig, error) {
	var el entConfigList
	if err := c.get("/clip/v2/resource/entertainment_configuration", &el); err != nil {
		return nil, err
	}
	sort.Slice(el.Data, func(i, j int) bool { return el.Data[i].ID < el.Data[j].ID })
	return el.Data, nil
}
