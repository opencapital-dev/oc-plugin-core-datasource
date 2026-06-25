package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/opencapital-dev/oc-plugin-sdk/computeclient"
	"github.com/opencapital-dev/oc-plugin-sdk/computeframe"

	"github.com/portfoliomangement/query-service/pkg/models"
)

// Compile-time interface assertions.
var (
	_ backend.QueryDataHandler    = (*Datasource)(nil)
	_ backend.CheckHealthHandler  = (*Datasource)(nil)
	_ backend.CallResourceHandler = (*Datasource)(nil)
)

// Datasource is a thin proxy to the local Python compute sidecar: it resolves
// the panel's code (inline or metric ref), substitutes dashboard variables, and
// posts {source, window} to the sidecar. The sidecar runs the code and returns
// the neutral frame; the backend converts it to a Grafana data.Frame.
type Datasource struct {
	settings    *models.PluginSettings
	compute     *computeclient.Client
	installRoot string
	baseFuncDir string
}

// NewDatasource is the SDK factory. Grafana calls it once per
// datasource-instance config; Dispose runs when settings change.
func NewDatasource(_ context.Context, src backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	settings, err := models.LoadPluginSettings(src)
	if err != nil {
		return nil, err
	}

	installRoot, err := pluginsInstallRoot(settings.PluginsInstallDir)
	if err != nil {
		log.DefaultLogger.Warn("core-datasource: could not derive plugins install root; metric refs will fail", "error", err)
	}

	baseFuncDir := ""
	if exe, err := os.Executable(); err == nil {
		baseFuncDir = filepath.Join(filepath.Dir(exe), "functions")
	}

	return &Datasource{
		settings:    settings,
		compute:     computeclient.New(settings.ComputeURL, &http.Client{Timeout: 60 * time.Second}),
		installRoot: installRoot,
		baseFuncDir: baseFuncDir,
	}, nil
}

// Dispose is a no-op: the datasource holds no long-lived connections.
func (d *Datasource) Dispose() {}

// QueryData fans out per RefID. One query = one metric eval.
func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	resp := backend.NewQueryDataResponse()
	for _, q := range req.Queries {
		resp.Responses[q.RefID] = d.query(ctx, q)
	}
	return resp, nil
}

// panelModel is the JSON shape stored in the panel's query JSON. `source` is an
// optional inline override; `ref` names a shipped metric; `vars` carries the
// frontend-resolved dashboard variables for backend substitution.
type panelModel struct {
	Source string            `json:"source"`
	Ref    string            `json:"ref"`
	Vars   map[string]string `json:"vars"`
}

func (d *Datasource) query(ctx context.Context, q backend.DataQuery) backend.DataResponse {
	var pm panelModel
	if err := json.Unmarshal(q.JSON, &pm); err != nil {
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("json unmarshal: %v", err))
	}

	code, err := selectCode(pm, d.installRoot)
	if err != nil {
		return backend.ErrDataResponse(backend.StatusBadRequest, err.Error())
	}
	code = substituteVars(code, pm.Vars)
	full := loadFunctions(d.baseFuncDir, d.installRoot, pm.Ref) + code

	from, to := q.TimeRange.From.UnixMicro(), q.TimeRange.To.UnixMicro()

	frame, err := d.compute.Compute(ctx, full, from, to)
	if err != nil {
		var ce *computeclient.ComputeError
		if errors.As(err, &ce) {
			return backend.ErrDataResponse(statusFor(ce.Status), fmt.Sprintf("compute: %s", ce.Message))
		}
		return backend.ErrDataResponse(backend.StatusInternal, fmt.Sprintf("compute: %v", err))
	}
	return backend.DataResponse{Frames: data.Frames{computeframe.ToFrame(frame)}}
}

// statusFor maps a sidecar HTTP status to a Grafana query status: an author
// error (400) surfaces as a bad-request query error; anything else as internal.
func statusFor(httpStatus int) backend.Status {
	if httpStatus == http.StatusBadRequest {
		return backend.StatusBadRequest
	}
	return backend.StatusInternal
}

// CheckHealth confirms the datasource is provisioned with a compute sidecar URL.
func (d *Datasource) CheckHealth(_ context.Context, _ *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	if d.settings.ComputeURL == "" {
		return errHealth("computeUrl is not configured"), nil
	}
	return &backend.CheckHealthResult{
		Status:  backend.HealthStatusOk,
		Message: "Data source is working",
	}, nil
}

func errHealth(msg string) *backend.CheckHealthResult {
	return &backend.CheckHealthResult{Status: backend.HealthStatusError, Message: msg}
}

// CallResource serves the shipped Python for a metric ref so the query editor
// can display it and reset overrides. GET metric-source?ref=pluginID/metric.
func (d *Datasource) CallResource(_ context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	u, err := url.Parse(req.URL)
	if err != nil {
		return sendJSON(sender, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	switch strings.TrimPrefix(u.Path, "/") {
	case "metric-source":
		src, err := readMetricSource(d.installRoot, u.Query().Get("ref"))
		if err != nil {
			return sendJSON(sender, http.StatusNotFound, map[string]string{"error": err.Error()})
		}
		return sendJSON(sender, http.StatusOK, map[string]string{"source": src})
	default:
		return sender.Send(&backend.CallResourceResponse{Status: http.StatusNotFound})
	}
}

func sendJSON(sender backend.CallResourceResponseSender, status int, body any) error {
	b, _ := json.Marshal(body)
	return sender.Send(&backend.CallResourceResponse{
		Status:  status,
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    b,
	})
}
