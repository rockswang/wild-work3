package meta

import (
	"encoding/json"
	"testing"
)

func TestManifestDefaultChannelsRoundTrip(t *testing.T) {
	original := Manifest{ID: "provider", DefaultChannels: []DefaultChannel{{ID: "provider", Name: "Provider", Config: map[string]any{"region": "auto"}}}}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Manifest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.DefaultChannels) != 1 || decoded.DefaultChannels[0].ID != "provider" || decoded.DefaultChannels[0].Name != "Provider" || decoded.DefaultChannels[0].Config["region"] != "auto" {
		t.Fatalf("invalid declaration: %+v", decoded.DefaultChannels)
	}
}
