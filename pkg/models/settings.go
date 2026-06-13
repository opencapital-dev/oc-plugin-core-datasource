package models

import (
	"encoding/json"
	"fmt"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/ignacioballester/oc-plugin-sdk/pluginclient"
)

// PluginSettings holds all configuration the core-datasource datasource backend
// needs. Non-secret fields come from jsonData; platformToken comes from
// DecryptedSecureJSONData so it is delivered per-plugin by the Grafana SDK
// and never shared via the process environment.
type PluginSettings struct {
	// ComputeURL is the base URL of the local Python compute sidecar the
	// backend posts {source, jwt, window} to. Configured via jsonData.computeUrl.
	ComputeURL string `json:"computeUrl"`

	// pluginclient fields — mirror AppJSONData from lib/pluginclient.
	PluginID         string `json:"pluginId"`
	OrgID            string `json:"orgId"`
	ControlPlaneURL  string `json:"controlPlaneUrl"`
	GatewayURL       string `json:"gatewayUrl"`
	InstanceTokenURL string `json:"instanceTokenUrl"`

	// PluginsRoot is the writable root for plugin SQLite. OpenReadOnlyForeign
	// resolves other plugins' DB paths under it; must match the root writer
	// plugins use. Empty => pluginclient env/default fallback.
	PluginsRoot string `json:"pluginsRoot"`

	// PluginsInstallDir is the read-only root that contains every installed
	// plugin's directory; the datasource reads `<dir>/<pluginID>/library-panels/
	// <metric>.py` for metric refs. Empty => derived from the backend binary's
	// own location. Configured via jsonData.pluginsInstallDir.
	PluginsInstallDir string `json:"pluginsInstallDir"`

	// PlatformToken and PluginTokens are loaded from DecryptedSecureJSONData.
	PlatformToken string            `json:"-"`
	PluginTokens  map[string]string `json:"-"`
}

// LoadPluginSettings parses jsonData and DecryptedSecureJSONData from the
// DataSourceInstanceSettings the Grafana SDK provides at instance creation.
// platformToken is read from DecryptedSecureJSONData["platformToken"]; all
// other fields are decoded from JSONData.
func LoadPluginSettings(source backend.DataSourceInstanceSettings) (*PluginSettings, error) {
	settings := PluginSettings{}
	if len(source.JSONData) > 0 {
		if err := json.Unmarshal(source.JSONData, &settings); err != nil {
			return nil, fmt.Errorf("could not unmarshal PluginSettings json: %w", err)
		}
	}
	settings.PlatformToken = source.DecryptedSecureJSONData["platformToken"]

	if raw := source.DecryptedSecureJSONData["pluginTokens"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &settings.PluginTokens); err != nil {
			return nil, fmt.Errorf("unmarshal pluginTokens: %w", err)
		}
	}
	if settings.PluginTokens == nil {
		settings.PluginTokens = map[string]string{}
	}

	// Default pluginId so existing single-plugin deploys don't need an
	// explicit jsonData entry.
	if settings.PluginID == "" {
		settings.PluginID = "core-datasource"
	}

	return &settings, nil
}

// PluginClientSettings returns the pluginclient.Settings the datasource
// backend passes to pluginclient.NewFromSettings. It re-encodes the
// non-secret jsonData fields so pluginclient can unmarshal them via
// its own AppJSONData struct.
func (s *PluginSettings) PluginClientSettings() pluginclient.Settings {
	// Re-encode only the fields pluginclient.AppJSONData knows about.
	// The original source.JSONData may contain extra keys (e.g. computeUrl)
	// that pluginclient ignores via json.Unmarshal, but we reconstruct a
	// minimal object to be explicit.
	jd, _ := json.Marshal(map[string]string{
		"pluginId":         s.PluginID,
		"orgId":            s.OrgID,
		"controlPlaneUrl":  s.ControlPlaneURL,
		"gatewayUrl":       s.GatewayURL,
		"instanceTokenUrl": s.InstanceTokenURL,
		"pluginsRoot":      s.PluginsRoot,
	})
	return pluginclient.Settings{
		JSONData: jd,
		DecryptedSecureJSONData: map[string]string{
			"platformToken": s.PlatformToken,
		},
	}
}
