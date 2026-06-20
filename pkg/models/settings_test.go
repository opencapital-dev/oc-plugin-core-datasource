package models

import (
	"encoding/json"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

func makeSettings(jsonData map[string]any, secureData map[string]string) backend.DataSourceInstanceSettings {
	jd, _ := json.Marshal(jsonData)
	return backend.DataSourceInstanceSettings{
		JSONData:                jd,
		DecryptedSecureJSONData: secureData,
	}
}

func TestLoadPluginSettings_ComputeURL(t *testing.T) {
	s, err := LoadPluginSettings(makeSettings(
		map[string]any{"computeUrl": "http://127.0.0.1:8790"},
		nil,
	))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.ComputeURL != "http://127.0.0.1:8790" {
		t.Errorf("ComputeURL = %q, want %q", s.ComputeURL, "http://127.0.0.1:8790")
	}
}

func TestLoadPluginSettings_PluginsInstallDir(t *testing.T) {
	s, err := LoadPluginSettings(makeSettings(
		map[string]any{"pluginsInstallDir": "/x/plugins"},
		nil,
	))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.PluginsInstallDir != "/x/plugins" {
		t.Errorf("PluginsInstallDir = %q, want %q", s.PluginsInstallDir, "/x/plugins")
	}
}

func TestLoadPluginSettings_Empty(t *testing.T) {
	s, err := LoadPluginSettings(makeSettings(nil, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.ComputeURL != "" {
		t.Errorf("ComputeURL should be empty, got %q", s.ComputeURL)
	}
}

func TestLoadPluginSettings_MalformedJSON(t *testing.T) {
	src := backend.DataSourceInstanceSettings{
		JSONData: []byte(`not-json`),
	}
	_, err := LoadPluginSettings(src)
	if err == nil {
		t.Error("expected error for malformed JSONData, got nil")
	}
}
