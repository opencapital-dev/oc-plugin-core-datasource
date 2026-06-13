package plugin

import (
	"database/sql"
	"testing"

	"github.com/ignacioballester/oc-plugin-sdk/dsl"

	_ "github.com/mutecomm/go-sqlcipher/v4"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`CREATE TABLE thing_base (
		portfolio    TEXT NOT NULL,
		instrument_id TEXT NOT NULL,
		ts           INTEGER NOT NULL,
		sector       TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	_, err = db.Exec(`CREATE VIEW gw_thing AS
		SELECT portfolio, instrument_id, ts, sector FROM thing_base`)
	if err != nil {
		t.Fatalf("create view: %v", err)
	}

	rows := []struct {
		portfolio, instrument, sector string
		ts                            int64
	}{
		{"p1", "AAPL", "Technology", 1000},
		{"p1", "AAPL", "Technology", 2000},
		{"p1", "MSFT", "Technology", 1000},
		{"p1", "MSFT", "Software", 2000},
		{"p2", "AAPL", "Technology", 1000},
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO thing_base VALUES (?, ?, ?, ?)`,
			r.portfolio, r.instrument, r.ts, r.sector,
		); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	return db
}

func TestIntrospectView(t *testing.T) {
	db := newTestDB(t)

	cols, err := introspectView(db, "gw_thing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"portfolio", "instrument_id", "ts", "sector"}
	if len(cols) != len(want) {
		t.Fatalf("got %v, want %v", cols, want)
	}
	for i, c := range cols {
		if c != want[i] {
			t.Errorf("col[%d]: got %q, want %q", i, c, want[i])
		}
	}
}

func TestIntrospectViewMissing(t *testing.T) {
	db := newTestDB(t)
	_, err := introspectView(db, "gw_nonexistent")
	if err == nil {
		t.Fatal("expected error for missing view, got nil")
	}
}

func TestCompileSQLite_Latest(t *testing.T) {
	db := newTestDB(t)
	cols, _ := introspectView(db, "gw_thing")

	sel, err := dsl.Parse(`thing{portfolio="p1"}@latest`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	sqlStr, args, err := compileSQLite("gw_thing", cols, sel, 0, 9999)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	wantSQL := `SELECT portfolio, instrument_id, ts, sector FROM (SELECT portfolio, instrument_id, ts, sector, ROW_NUMBER() OVER (PARTITION BY portfolio, instrument_id ORDER BY ts DESC) AS _rn FROM gw_thing WHERE portfolio = ? AND ts <= ?) WHERE _rn = 1`
	if sqlStr != wantSQL {
		t.Errorf("SQL mismatch\ngot:  %s\nwant: %s", sqlStr, wantSQL)
	}

	// Run against fixture DB and verify deduplication.
	frame, err := readRows(t.Context(), db, sqlStr, args)
	if err != nil {
		t.Fatalf("readRows: %v", err)
	}
	// p1 has 2 instruments → 2 rows (latest ts each).
	if len(frame.Rows) != 2 {
		t.Errorf("expected 2 rows (one per instrument), got %d", len(frame.Rows))
	}
	for _, row := range frame.Rows {
		// Latest ts for both is 2000.
		if row[2] != int64(2000) {
			t.Errorf("expected latest ts=2000, got %v", row[2])
		}
	}
}

func TestCompileSQLite_Window(t *testing.T) {
	db := newTestDB(t)
	cols, _ := introspectView(db, "gw_thing")

	sel, err := dsl.Parse(`thing{portfolio="p1"}@window`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	sqlStr, args, err := compileSQLite("gw_thing", cols, sel, 500, 1500)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Window should produce ts >= ? AND ts <= ?.
	frame, err := readRows(t.Context(), db, sqlStr, args)
	if err != nil {
		t.Fatalf("readRows: %v", err)
	}
	// p1, ts in [500,1500]: AAPL@1000 + MSFT@1000 → 2 rows.
	if len(frame.Rows) != 2 {
		t.Errorf("expected 2 rows in window, got %d", len(frame.Rows))
	}
}

func TestCompileSQLite_RegexError(t *testing.T) {
	db := newTestDB(t)
	cols, _ := introspectView(db, "gw_thing")

	sel, err := dsl.Parse(`thing{portfolio=~"p.*"}@latest`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	_, _, err = compileSQLite("gw_thing", cols, sel, 0, 9999)
	if err == nil {
		t.Fatal("expected error for regex matcher on SQLite, got nil")
	}
}

func TestCompileSQLite_UnknownLabel(t *testing.T) {
	db := newTestDB(t)
	cols, _ := introspectView(db, "gw_thing")

	// "sector" is a value column, not grain — must be rejected as matcher.
	sel, err := dsl.Parse(`thing{sector="Technology"}@latest`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	_, _, err = compileSQLite("gw_thing", cols, sel, 0, 9999)
	if err == nil {
		t.Fatal("expected error for non-grain matcher label, got nil")
	}
}
