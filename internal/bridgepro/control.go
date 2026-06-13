package bridgepro

// SetLight setzt den Zustand einer Lampe (CLIP-v2-PUT auf die light-Ressource).
func (c *Client) SetLight(uuid string, v2body map[string]any) error {
	return c.put("/clip/v2/resource/light/"+uuid, v2body)
}

// SetGroupedLight setzt den Zustand eines grouped_light (Raum/Zone-Sammelsteuerung).
func (c *Client) SetGroupedLight(uuid string, v2body map[string]any) error {
	return c.put("/clip/v2/resource/grouped_light/"+uuid, v2body)
}
