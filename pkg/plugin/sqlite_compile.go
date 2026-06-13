package plugin

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/ignacioballester/oc-plugin-sdk/dsl"
)

// introspectView returns the column names of a gw_ view in declaration order
// via PRAGMA table_info. Error if the view is missing or has zero columns.
func introspectView(db *sql.DB, view string) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", view))
	if err != nil {
		return nil, fmt.Errorf("PRAGMA table_info(%s): %w", view, err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("view %q not found or has no columns", view)
	}
	return cols, nil
}

// compileSQLite compiles a parsed DSL selector to a parameterized SQLite query
// against a gw_ view. The view's column layout drives the grain/ts/values split
// by introspection convention: cols before ts = grain, ts = timestamp, cols
// after ts = values. from/to are epoch microseconds.
//
// Regex operators (=~ / !~) are rejected — those are RisingWave-only.
func compileSQLite(view string, cols []string, s dsl.Selector, from, to int64) (string, []any, error) {
	// Derive grain, tsCol, values from column order.
	tsIdx := -1
	for i, c := range cols {
		if c == "ts" {
			tsIdx = i
			break
		}
	}

	var grain []string
	var tsCol string
	if tsIdx >= 0 {
		grain = cols[:tsIdx]
		tsCol = "ts"
	}

	grainSet := make(map[string]bool, len(grain))
	for _, g := range grain {
		grainSet[g] = true
	}

	var where []string
	var args []any

	for _, m := range s.Strings {
		if !grainSet[m.Label] {
			return "", nil, fmt.Errorf("unknown/unsupported matcher label %q (must be a grain column)", m.Label)
		}
		switch m.Op {
		case dsl.OpEq:
			where = append(where, fmt.Sprintf("%s = ?", m.Label))
			args = append(args, m.Value)
		case dsl.OpNe:
			where = append(where, fmt.Sprintf("%s <> ?", m.Label))
			args = append(args, m.Value)
		default:
			return "", nil, fmt.Errorf("regex matchers are RisingWave-only (label %q)", m.Label)
		}
	}

	for _, m := range s.Numbers {
		where = append(where, fmt.Sprintf("%s %s ?", m.Col, numSQLite(m.Op)))
		args = append(args, m.Value)
	}

	if tsCol != "" {
		switch s.Mode {
		case dsl.Window:
			where = append(where, fmt.Sprintf("%s >= ?", tsCol))
			args = append(args, from)
			where = append(where, fmt.Sprintf("%s <= ?", tsCol))
			args = append(args, to)
		default: // Asof and Latest: upper-bound only
			where = append(where, fmt.Sprintf("%s <= ?", tsCol))
			args = append(args, to)
		}
	}

	colList := strings.Join(cols, ", ")
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}

	if s.Mode == dsl.Latest && len(grain) > 0 && tsCol != "" {
		grainList := strings.Join(grain, ", ")
		inner := fmt.Sprintf(
			"SELECT %s, ROW_NUMBER() OVER (PARTITION BY %s ORDER BY %s DESC) AS _rn FROM %s%s",
			colList, grainList, tsCol, view, whereSQL,
		)
		return fmt.Sprintf("SELECT %s FROM (%s) WHERE _rn = 1", colList, inner), args, nil
	}

	order := ""
	if tsCol != "" {
		order = " ORDER BY " + tsCol + " ASC"
	}
	return fmt.Sprintf("SELECT %s FROM %s%s%s", colList, view, whereSQL, order), args, nil
}

func numSQLite(op dsl.NumOp) string {
	switch op {
	case dsl.NumEq:
		return "="
	case dsl.NumNe:
		return "<>"
	case dsl.NumGt:
		return ">"
	case dsl.NumGe:
		return ">="
	case dsl.NumLt:
		return "<"
	case dsl.NumLe:
		return "<="
	}
	return "="
}
