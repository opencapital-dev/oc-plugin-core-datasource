package models

import (
	"encoding/json"
	"fmt"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

// PluginSettings holds all configuration the core-datasource datasource backend
// needs. Non-secret fields come from jsonData.
type PluginSettings struct {
	// ComputeURL is the base URL of the local Python compute sidecar the
	// backend posts {source, window} to. Configured via jsonData.computeUrl.
	ComputeURL string `json:"computeUrl"`

	// PluginsInstallDir is the read-only root that contains every installed
	// plugin's directory; the datasource reads `<dir>/<pluginID>/library-panels/
	// <metric>.py` for metric refs. Empty => derived from the backend binary's
	// own location. Configured via jsonData.pluginsInstallDir.
	PluginsInstallDir string `json:"pluginsInstallDir"`
}

// LoadPluginSettings parses jsonData from the DataSourceInstanceSettings the
// Grafana SDK provides at instance creation.
func LoadPluginSettings(source backend.DataSourceInstanceSettings) (*PluginSettings, error) {
	settings := PluginSettings{}
	if len(source.JSONData) > 0 {
		if err := json.Unmarshal(source.JSONData, &settings); err != nil {
			return nil, fmt.Errorf("could not unmarshal PluginSettings json: %w", err)
		}
	}
	return &settings, nil
}
