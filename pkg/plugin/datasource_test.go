package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/ignacioballester/oc-plugin-sdk/computeclient"

	_ "github.com/mutecomm/go-sqlcipher/v4"

	"github.com/portfoliomangement/query-service/pkg/models"
)

// stubDatasource builds a Datasource whose compute client points at a stub
// sidecar. pc is nil, so mintJWT returns "" (unauthenticated path) — the
// existing backend tests exercise this same unauthenticated mode.
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

// --- hasPluginPrefix ---

func TestHasPluginPrefix(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{`sectors = yfinance-app/classification{portfolio="p1"} @latest`, true},
		{`nav{portfolio="p1"} @window`, false},
		{``, false},
		{`foo/bar{}`, true},
		{`foobar{}`, false},
		{`no_slash`, false},
	}
	for _, c := range cases {
		if got := hasPluginPrefix(c.src); got != c.want {
			t.Errorf("hasPluginPrefix(%q) = %v, want %v", c.src, got, c.want)
		}
	}
}

// --- resolvePrefetched ---

// classificationDB returns an in-memory SQLite with a gw_classification view
// matching the yfinance schema: portfolio, instrument_id, ts, sector.
func classificationDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`CREATE TABLE classification_base (
		portfolio     TEXT NOT NULL,
		instrument_id TEXT NOT NULL,
		ts            INTEGER NOT NULL,
		sector        TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	_, err = db.Exec(`CREATE VIEW gw_classification AS
		SELECT portfolio, instrument_id, ts, sector FROM classification_base`)
	if err != nil {
		t.Fatalf("create view: %v", err)
	}
	_, err = db.Exec(`INSERT INTO classification_base VALUES
		("p1", "AAPL", 1000, "Technology"),
		("p1", "MSFT", 1000, "Software")`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return db
}

func TestResolvePrefetched_HappyPath(t *testing.T) {
	db := classificationDB(t)

	openCalls := 0
	open := func(_ context.Context, pluginID string) (*sql.DB, error) {
		if pluginID != "yfinance-app" {
			t.Errorf("open called with unexpected pluginID %q", pluginID)
		}
		openCalls++
		return db, nil
	}

	bindings := map[string]string{
		"sectors": `yfinance-app/classification{portfolio="p1"} @latest`,
		"navs":    `nav{portfolio="p1"} @window`,
	}

	prefetched, err := resolvePrefetched(context.Background(), bindings, open, 0, 9999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the prefixed binding should be resolved.
	if _, ok := prefetched["sectors"]; !ok {
		t.Error("expected 'sectors' in prefetched map")
	}
	if _, ok := prefetched["navs"]; ok {
		t.Error("unprefixed 'navs' should not be in prefetched map")
	}
	// open must have been called exactly once.
	if openCalls != 1 {
		t.Errorf("open called %d times, want 1", openCalls)
	}
	// Frame must have the view's columns.
	frame := prefetched["sectors"]
	wantCols := []string{"portfolio", "instrument_id", "ts", "sector"}
	if len(frame.Columns) != len(wantCols) {
		t.Fatalf("columns %v, want %v", frame.Columns, wantCols)
	}
	for i, c := range frame.Columns {
		if c != wantCols[i] {
			t.Errorf("col[%d] %q want %q", i, c, wantCols[i])
		}
	}
	// Two rows inserted for p1.
	if len(frame.Rows) != 2 {
		t.Errorf("rows %d, want 2", len(frame.Rows))
	}
}

func TestResolvePrefetched_OpenError(t *testing.T) {
	open := func(_ context.Context, pluginID string) (*sql.DB, error) {
		return nil, fmt.Errorf("disk full")
	}

	bindings := map[string]string{
		"sectors": `yfinance-app/classification{portfolio="p1"} @latest`,
	}

	_, err := resolvePrefetched(context.Background(), bindings, open, 0, 9999)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	errMsg := err.Error()
	if !contains(errMsg, "yfinance-app") {
		t.Errorf("error %q should mention plugin id", errMsg)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
