package config

import (
	"path/filepath"
	"testing"
)

func TestIdentity_Derivations(t *testing.T) {
	// Given
	id := Identity{Serial: "2c4d54ea2832"}

	// Then
	if got := id.MAC(); got != "2c:4d:54:ea:28:32" {
		t.Errorf("MAC = %q", got)
	}
	if got := id.BridgeID(); got != "2C4D54FFFEEA2832" {
		t.Errorf("BridgeID = %q", got)
	}
	if got := id.UUID(); got != "2f402f80-da50-11e1-9b23-2c4d54ea2832" {
		t.Errorf("UUID = %q", got)
	}
}

func TestLoad_GeneratesAndPersistsIdentity(t *testing.T) {
	// Given
	path := filepath.Join(t.TempDir(), "relume.json")

	// When
	c1, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c1.Identity.Serial) != 12 {
		t.Fatalf("serial length = %d", len(c1.Identity.Serial))
	}

	// Then: erneutes Laden liefert dieselbe Identität (stabil/persistiert)
	c2, err := Load(path)
	if err != nil {
		t.Fatalf("Load 2: %v", err)
	}
	if c1.Identity.Serial != c2.Identity.Serial {
		t.Errorf("serial nicht stabil: %q != %q", c1.Identity.Serial, c2.Identity.Serial)
	}
}

func TestAddApiUser_Persists(t *testing.T) {
	// Given
	path := filepath.Join(t.TempDir(), "relume.json")
	c, _ := Load(path)

	// When
	if err := c.AddApiUser(&ApiUser{Username: "abc123", DeviceType: "TV#x"}); err != nil {
		t.Fatalf("AddApiUser: %v", err)
	}

	// Then
	reloaded, _ := Load(path)
	if !reloaded.HasApiUser("abc123") {
		t.Error("ApiUser nicht persistiert")
	}
}
