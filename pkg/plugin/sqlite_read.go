package plugin

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ignacioballester/oc-plugin-sdk/computeclient"
)

// readRows executes sqlStr with args and maps the result set into a PrefetchedFrame.
// []byte cells are coerced to string; all other types pass through as-is.
func readRows(ctx context.Context, db *sql.DB, sqlStr string, args []any) (computeclient.PrefetchedFrame, error) {
	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return computeclient.PrefetchedFrame{}, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	colNames, err := rows.Columns()
	if err != nil {
		return computeclient.PrefetchedFrame{}, err
	}

	// Rows starts as a non-nil empty slice: a nil slice marshals to JSON null,
	// which the compute sidecar rejects ("prefetched[...] must have ... rows").
	// An empty foreign result must serialise as [], not null.
	out := computeclient.PrefetchedFrame{Columns: colNames, Rows: make([][]any, 0)}

	for rows.Next() {
		cells := make([]any, len(colNames))
		ptrs := make([]any, len(colNames))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return computeclient.PrefetchedFrame{}, err
		}
		row := make([]any, len(colNames))
		for i, v := range cells {
			if b, ok := v.([]byte); ok {
				row[i] = string(b)
			} else {
				row[i] = v
			}
		}
		out.Rows = append(out.Rows, row)
	}
	return out, rows.Err()
}
