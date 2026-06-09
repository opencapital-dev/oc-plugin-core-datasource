package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/portfolio-management/computeclient"
	"github.com/portfolio-management/computeframe"
	"github.com/portfolio-management/dsl"
	"github.com/portfolio-management/pluginclient"

	"github.com/portfoliomangement/query-service/pkg/models"
)

// Compile-time interface assertions.
var (
	_ backend.QueryDataHandler   = (*Datasource)(nil)
	_ backend.CheckHealthHandler = (*Datasource)(nil)
)

// Datasource is a thin proxy to the local Python compute sidecar: it mints the
// per-(plugin, org) read-gateway JWT, takes the dashboard time range, and posts
// {source, jwt, window} to the sidecar. The sidecar fetches the rows and runs
// the source; the backend frames the returned neutral result.
type Datasource struct {
	settings    *models.PluginSettings
	pc          *pluginclient.Client
	compute     *computeclient.Client
	installRoot string
}

// NewDatasource is the SDK factory. Grafana calls it once per
// datasource-instance config; Dispose runs when settings change.
func NewDatasource(_ context.Context, src backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	settings, err := models.LoadPluginSettings(src)
	if err != nil {
		return nil, err
	}

	var pc *pluginclient.Client
	if settings.PlatformToken == "" {
		log.DefaultLogger.Warn("core-datasource: platformToken absent from secureJsonData — running unauthenticated; seed a plugin_installs row + platformToken for core-datasource in the control plane")
	} else {
		pc, err = pluginclient.NewFromSettings(settings.PluginClientSettings())
		if err != nil {
			log.DefaultLogger.Warn("core-datasource: pluginclient init skipped (unauthenticated mode)", "error", err)
			pc = nil
		}
	}

	installRoot, err := pluginsInstallRoot(settings.PluginsInstallDir)
	if err != nil {
		log.DefaultLogger.Warn("core-datasource: could not derive plugins install root; metric refs will fail", "error", err)
	}

	return &Datasource{
		settings:    settings,
		pc:          pc,
		compute:     computeclient.New(settings.ComputeURL, &http.Client{Timeout: 60 * time.Second}),
		installRoot: installRoot,
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

	jwt, org, err := d.jwtAndOrg(ctx)
	if err != nil {
		return backend.ErrDataResponse(backend.StatusInternal, fmt.Sprintf("mint read-gateway jwt: %v", err))
	}

	from, to := q.TimeRange.From.UnixMicro(), q.TimeRange.To.UnixMicro()

	if !hasPluginPrefix(code) {
		frame, err := d.compute.Compute(ctx, code, jwt, from, to)
		if err != nil {
			var ce *computeclient.ComputeError
			if errors.As(err, &ce) {
				return backend.ErrDataResponse(statusFor(ce.Status), fmt.Sprintf("compute: %s", ce.Message))
			}
			return backend.ErrDataResponse(backend.StatusInternal, fmt.Sprintf("compute: %v", err))
		}
		return backend.DataResponse{Frames: data.Frames{computeframe.ToFrame(frame)}}
	}

	// Slow path: source has at least one plugin-prefixed binding.
	if d.pc == nil {
		return backend.ErrDataResponse(backend.StatusInternal, "core-datasource: pluginclient not initialised — provision platformToken to read foreign plugin SQLite")
	}
	if org == uuid.Nil {
		return backend.ErrDataResponse(backend.StatusInternal, "core-datasource: org id unknown — set orgId in jsonData or ensure the request identity carries one")
	}

	bindings, err := d.compute.Plan(ctx, code)
	if err != nil {
		var ce *computeclient.ComputeError
		if errors.As(err, &ce) {
			return backend.ErrDataResponse(statusFor(ce.Status), fmt.Sprintf("plan: %s", ce.Message))
		}
		return backend.ErrDataResponse(backend.StatusInternal, fmt.Sprintf("plan: %v", err))
	}

	open := func(ctx context.Context, pluginID string) (*sql.DB, error) {
		tok := d.settings.PluginTokens[pluginID]
		if tok == "" {
			return nil, fmt.Errorf("no platform token provisioned for plugin %q", pluginID)
		}
		return d.pc.OpenReadOnlyForeign(ctx, pluginID, tok, org)
	}

	prefetched, err := resolvePrefetched(ctx, bindings, open, from, to)
	if err != nil {
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("resolve bindings: %v", err))
	}

	frame, err := d.compute.ComputeWithPrefetched(ctx, code, jwt, from, to, prefetched)
	if err != nil {
		var ce *computeclient.ComputeError
		if errors.As(err, &ce) {
			return backend.ErrDataResponse(statusFor(ce.Status), fmt.Sprintf("compute: %s", ce.Message))
		}
		return backend.ErrDataResponse(backend.StatusInternal, fmt.Sprintf("compute: %v", err))
	}
	return backend.DataResponse{Frames: data.Frames{computeframe.ToFrame(frame)}}
}

// jwtAndOrg fetches the per-request identity once, returning the session JWT
// and org UUID together so the caller avoids a second round-trip. When
// pluginclient is absent (unauthenticated mode), jwt is "" and org is uuid.Nil;
// the settings OrgID fallback is applied so that org is valid even when the
// request identity is absent.
func (d *Datasource) jwtAndOrg(ctx context.Context) (jwt string, org uuid.UUID, _ error) {
	if d.pc == nil {
		if d.settings.OrgID != "" {
			o, err := uuid.Parse(d.settings.OrgID)
			if err != nil {
				return "", uuid.Nil, fmt.Errorf("settings orgId: %w", err)
			}
			return "", o, nil
		}
		return "", uuid.Nil, nil
	}
	authCtx, err := d.pc.WithRequest(ctx, nil)
	if err != nil {
		return "", uuid.Nil, err
	}
	id, err := pluginclient.IdentityFrom(authCtx)
	if err != nil {
		return "", uuid.Nil, err
	}
	// Prefer identity org; fall back to configured OrgID.
	org = id.OrgID
	if org == uuid.Nil && d.settings.OrgID != "" {
		if o, err := uuid.Parse(d.settings.OrgID); err == nil {
			org = o
		}
	}
	return id.SessionJWT, org, nil
}

// pluginPrefixRe matches `<ident>/<ident>{` which signals a plugin-scoped
// binding in a source string. False positives are harmless; false negatives
// would skip the Plan round-trip and silently miss prefetched data.
var pluginPrefixRe = regexp.MustCompile(`[A-Za-z0-9_]+/[A-Za-z0-9_]+\s*\{`)

// hasPluginPrefix reports whether source may contain a pluginID/entity binding.
func hasPluginPrefix(source string) bool {
	return pluginPrefixRe.MatchString(source)
}

// openFunc opens a foreign plugin's read-only SQLite for the given plugin id.
type openFunc func(ctx context.Context, pluginID string) (*sql.DB, error)

// resolvePrefetched resolves every plugin-prefixed binding to a frame by
// reading the owning plugin's gw_<entity> view. Unprefixed bindings are
// left for the compute sidecar (skipped here). bindings is {param -> raw
// selector} from Plan().
func resolvePrefetched(ctx context.Context, bindings map[string]string, open openFunc, from, to int64) (map[string]computeclient.PrefetchedFrame, error) {
	prefetched := make(map[string]computeclient.PrefetchedFrame)
	for param, raw := range bindings {
		s, err := dsl.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("parse binding %q: %w", param, err)
		}
		if s.PluginID == "" {
			continue
		}
		db, err := open(ctx, s.PluginID)
		if err != nil {
			return nil, fmt.Errorf("open plugin %q: %w", s.PluginID, err)
		}
		view := "gw_" + s.Entity
		cols, err := introspectView(db, view)
		if err != nil {
			return nil, fmt.Errorf("entity %q not exposed by plugin %q: %w", s.Entity, s.PluginID, err)
		}
		sqlStr, args, err := compileSQLite(view, cols, s, from, to)
		if err != nil {
			return nil, fmt.Errorf("compile %q/%q: %w", s.PluginID, s.Entity, err)
		}
		frame, err := readRows(ctx, db, sqlStr, args)
		if err != nil {
			return nil, fmt.Errorf("read %q/%q: %w", s.PluginID, s.Entity, err)
		}
		prefetched[param] = frame
	}
	return prefetched, nil
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
