package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/opencapital-dev/oc-plugin-sdk/computeclient"

	"github.com/portfoliomangement/query-service/pkg/models"
)

// stubDatasource builds a Datasource whose compute client points at a stub
// sidecar. The datasource posts {source, window} and the handler returns a
// canned neutral frame.
func stubDatasource(t *testing.T, handler http.HandlerFunc) *Datasource {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Datasource{
		settings: &models.PluginSettings{ComputeURL: srv.URL},
		compute:  computeclient.New(srv.URL, srv.Client()),
	}
}

func queryWith(source string) backend.DataQuery {
	body, _ := json.Marshal(map[string]string{"source": source})
	return backend.DataQuery{
		RefID: "A",
		JSON:  body,
		TimeRange: backend.TimeRange{
			From: time.Unix(0, 0),
			To:   time.Unix(100, 0),
		},
	}
}

func TestQuery_Scalar(t *testing.T) {
	ds := stubDatasource(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["source"] != "total_return()" {
			t.Errorf("source=%v want total_return()", req["source"])
		}
		// Verify no jwt field is sent.
		if _, hasJWT := req["jwt"]; hasJWT {
			t.Error("request must not contain a jwt field")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":"scalar","columns":["value"],"rows":[[12.5]]}`))
	})

	resp := ds.query(context.Background(), queryWith("total_return()"))
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if len(resp.Frames) != 1 || len(resp.Frames[0].Fields) != 1 {
		t.Fatalf("frame shape %+v", resp.Frames)
	}
	v, ok := resp.Frames[0].Fields[0].At(0).(*float64)
	if !ok || v == nil || *v != 12.5 {
		t.Errorf("scalar=%v want 12.5", resp.Frames[0].Fields[0].At(0))
	}
}

func TestQuery_Series(t *testing.T) {
	ds := stubDatasource(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":"series","columns":["ts","value"],"rows":[[1700000000000000,1.0],[1700000060000000,2.0]]}`))
	})

	resp := ds.query(context.Background(), queryWith("cumulative_twr()"))
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if len(resp.Frames[0].Fields) != 2 {
		t.Fatalf("want 2 fields, got %d", len(resp.Frames[0].Fields))
	}
	if _, ok := resp.Frames[0].Fields[0].At(0).(time.Time); !ok {
		t.Errorf("ts field type %T want time.Time", resp.Frames[0].Fields[0].At(0))
	}
}

func TestQuery_400_CleanError(t *testing.T) {
	ds := stubDatasource(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"source error: name 'pnl' is not defined"}`))
	})

	resp := ds.query(context.Background(), queryWith("bad source"))
	if resp.Error == nil {
		t.Fatal("expected a query error for a 400 source error")
	}
	if resp.Status != backend.StatusBadRequest {
		t.Errorf("status=%v want StatusBadRequest", resp.Status)
	}
}

func TestQueryData_FansOut(t *testing.T) {
	ds := stubDatasource(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":"scalar","columns":["value"],"rows":[[1]]}`))
	})

	resp, err := ds.QueryData(context.Background(), &backend.QueryDataRequest{
		Queries: []backend.DataQuery{queryWith("x()")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Responses) != 1 {
		t.Fatalf("want 1 response, got %d", len(resp.Responses))
	}
}

// TestQuery_WindowForwarded verifies from/to are forwarded in the request body.
func TestQuery_WindowForwarded(t *testing.T) {
	var gotWindow map[string]any
	ds := stubDatasource(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if win, ok := req["window"].(map[string]any); ok {
			gotWindow = win
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":"scalar","columns":["v"],"rows":[[1]]}`))
	})

	body, _ := json.Marshal(map[string]string{"source": "f()"})
	q := backend.DataQuery{
		RefID: "A",
		JSON:  body,
		TimeRange: backend.TimeRange{
			From: time.Unix(1000, 0),
			To:   time.Unix(2000, 0),
		},
	}
	resp := ds.query(context.Background(), q)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if gotWindow == nil {
		t.Fatal("window not present in request body")
	}
	wantFrom := float64(time.Unix(1000, 0).UnixMicro())
	wantTo := float64(time.Unix(2000, 0).UnixMicro())
	if gotWindow["from"] != wantFrom {
		t.Errorf("window.from=%v want %v", gotWindow["from"], wantFrom)
	}
	if gotWindow["to"] != wantTo {
		t.Errorf("window.to=%v want %v", gotWindow["to"], wantTo)
	}
}

func TestComposeSourcePrependsHelpers(t *testing.T) {
	tmp := t.TempDir()
	base := tmp + "/ds/functions"
	writePy(t, base, "series.py", "def over_time(fn, step):\n    return None\n")
	panel := "@metric(output='series')\ndef m():\n    return over_time(lambda t: 1.0, '1d')\n"

	full := loadFunctions(base, tmp, "core-app/m") + substituteVars(panel, nil)
	if !strings.HasPrefix(full, "def over_time") {
		t.Fatal("helpers must be prepended before panel source")
	}
	if !strings.Contains(full, "def m()") {
		t.Fatal("panel source must be present")
	}
}

// TestQuery_VarSubstitution confirms vars are substituted before forwarding.
func TestQuery_VarSubstitution(t *testing.T) {
	var gotSource string
	ds := stubDatasource(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotSource, _ = req["source"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":"scalar","columns":["v"],"rows":[[1]]}`))
	})

	body, _ := json.Marshal(map[string]any{
		"source": `total_return(portfolio="$portfolio_id")`,
		"vars":   map[string]string{"portfolio_id": "demo"},
	})
	q := backend.DataQuery{
		RefID: "A",
		JSON:  body,
		TimeRange: backend.TimeRange{
			From: time.Unix(0, 0),
			To:   time.Unix(100, 0),
		},
	}
	resp := ds.query(context.Background(), q)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	want := `total_return(portfolio="demo")`
	if gotSource != want {
		t.Errorf("source forwarded=%q want %q", gotSource, want)
	}
}
