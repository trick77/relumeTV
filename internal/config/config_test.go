package config

import (
	"os"
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
	path := filepath.Join(t.TempDir(), "relumetv.json")

	// When
	c1, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c1.Identity.Serial) != 12 {
		t.Fatalf("serial length = %d", len(c1.Identity.Serial))
	}

	// Then: loading again returns the same identity (stable/persisted)
	c2, err := Load(path)
	if err != nil {
		t.Fatalf("Load 2: %v", err)
	}
	if c1.Identity.Serial != c2.Identity.Serial {
		t.Errorf("serial not stable: %q != %q", c1.Identity.Serial, c2.Identity.Serial)
	}
}

func TestLoad_StampsAndPersistsSchemaVersion(t *testing.T) {
	// Given: a fresh config
	path := filepath.Join(t.TempDir(), "relumetv.json")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Then: the current schema version is set and survives a reload
	if c.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", c.SchemaVersion, CurrentSchemaVersion)
	}
	data, _ := os.ReadFile(path)
	if !contains(data, []byte(`"schemaVersion"`)) {
		t.Errorf("schemaVersion not written to disk: %s", data)
	}
}

func TestLoad_MigratesLegacyZeroVersion(t *testing.T) {
	// Given: a legacy config with no schemaVersion field
	path := filepath.Join(t.TempDir(), "relumetv.json")
	if err := os.WriteFile(path, []byte(`{"identity":{"serial":"2c4d54ea2832"},"apiUsers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// When
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Then: it is adopted to the current version
	if c.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", c.SchemaVersion, CurrentSchemaVersion)
	}
}

func TestLoad_RejectsNewerSchemaVersion(t *testing.T) {
	// Given: a config written by a newer build
	path := filepath.Join(t.TempDir(), "relumetv.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":9999,"identity":{"serial":"2c4d54ea2832"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// When / Then: refuse rather than silently mishandle
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for newer schema version, got nil")
	}
}

func TestLoad_RemovesOrphanedTempFile(t *testing.T) {
	// Given: a real config plus a leftover .tmp from a crashed save
	dir := t.TempDir()
	path := filepath.Join(dir, "relumetv.json")
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	// When
	if _, err := Load(path); err != nil {
		t.Fatalf("Load 2: %v", err)
	}

	// Then: the orphaned temp file is gone
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("orphaned .tmp not removed (stat err = %v)", err)
	}
}

func contains(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}

func TestEntConfigID_RoundTripsAndClears(t *testing.T) {
	// Given
	path := filepath.Join(t.TempDir(), "relumetv.json")
	c, _ := Load(path)

	// When: save an id
	if err := c.SaveEntConfigID("cfg-uuid"); err != nil {
		t.Fatalf("SaveEntConfigID: %v", err)
	}

	// Then: it persists across a reload
	reloaded, _ := Load(path)
	if got := reloaded.LoadEntConfigID(); got != "cfg-uuid" {
		t.Fatalf("LoadEntConfigID = %q, want cfg-uuid", got)
	}

	// And: clearing it persists too
	if err := reloaded.SaveEntConfigID(""); err != nil {
		t.Fatalf("SaveEntConfigID clear: %v", err)
	}
	again, _ := Load(path)
	if got := again.LoadEntConfigID(); got != "" {
		t.Fatalf("LoadEntConfigID after clear = %q, want empty", got)
	}
}

func TestSetPro_DoesNotClobberEntConfigID(t *testing.T) {
	// Given: a persisted ent config id
	path := filepath.Join(t.TempDir(), "relumetv.json")
	c, _ := Load(path)
	if err := c.SaveEntConfigID("cfg-uuid"); err != nil {
		t.Fatalf("SaveEntConfigID: %v", err)
	}

	// When: the Pro pairing is replaced (copy-on-write, as watchPro/reconnect do)
	if err := c.SetPro(&BridgePro{Host: "10.0.0.5", AppKey: "k"}); err != nil {
		t.Fatalf("SetPro: %v", err)
	}

	// Then: the ent config id survives (it is top-level, not inside BridgePro)
	if got := c.LoadEntConfigID(); got != "cfg-uuid" {
		t.Fatalf("LoadEntConfigID = %q, want cfg-uuid (clobbered by SetPro)", got)
	}
}

func TestAddApiUser_Persists(t *testing.T) {
	// Given
	path := filepath.Join(t.TempDir(), "relumetv.json")
	c, _ := Load(path)

	// When
	if err := c.AddApiUser(&ApiUser{Username: "abc123", DeviceType: "TV#x"}); err != nil {
		t.Fatalf("AddApiUser: %v", err)
	}

	// Then
	reloaded, _ := Load(path)
	if !reloaded.HasApiUser("abc123") {
		t.Error("ApiUser not persisted")
	}
}
