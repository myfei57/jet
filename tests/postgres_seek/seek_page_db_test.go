//go:build postgres
// +build postgres

// Package postgres_seek contains real-database integration tests for the PostgreSQL
// SeekPage keyset pagination helper. It is fully self-contained: it creates its own table,
// so it does not depend on generated .gentestdata models.
package postgres_seek

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/go-jet/jet/v2/postgres"
	"github.com/go-jet/jet/v2/tests/dbconfig"
	"github.com/stretchr/testify/require"

	_ "github.com/lib/pq"
)

var (
	colID    = postgres.IntegerColumn("id")
	colGrp   = postgres.IntegerColumn("grp")
	colScore = postgres.IntegerColumn("score")
	colLabel = postgres.StringColumn("label")

	// The "seek_item" alias normalizes to the seekItem struct type name under qrm's
	// underscore-insensitive identifier matching.
	seekItems = postgres.NewTable("", "seek_page_items", "seek_item", colID, colGrp, colScore, colLabel)
)

// seekItem mirrors one row of seek_page_items. Pointer fields distinguish SQL NULL from zero.
type seekItem struct {
	ID    int32
	Grp   *int32
	Score *int32
	Label string
}

func i32(v int32) *int32 { return &v }

// seedRows is a fixed dataset. It deliberately contains:
//   - duplicate (grp, score) prefixes that must be resolved by the unique id,
//   - NULL groups on both nullable columns, including rows where both are NULL,
//   - duplicate labels.
var seedRows = []seekItem{
	{ID: 1, Grp: nil, Score: i32(10), Label: "a"},
	{ID: 2, Grp: nil, Score: nil, Label: "b"},
	{ID: 3, Grp: i32(1), Score: i32(10), Label: "c"},
	{ID: 4, Grp: i32(1), Score: nil, Label: "a"},
	{ID: 5, Grp: i32(1), Score: i32(10), Label: "b"},
	{ID: 6, Grp: i32(2), Score: i32(5), Label: "d"},
	{ID: 7, Grp: nil, Score: i32(5), Label: "e"},
	{ID: 8, Grp: i32(2), Score: nil, Label: "f"},
	{ID: 9, Grp: i32(2), Score: i32(5), Label: "g"},
	{ID: 10, Grp: i32(3), Score: nil, Label: "a"},
	{ID: 11, Grp: i32(3), Score: i32(7), Label: "h"},
	{ID: 12, Grp: nil, Score: nil, Label: "i"},
	{ID: 13, Grp: i32(1), Score: i32(5), Label: "j"},
	{ID: 14, Grp: i32(4), Score: nil, Label: "k"},
	{ID: 15, Grp: i32(4), Score: i32(5), Label: "l"},
	{ID: 16, Grp: i32(2), Score: i32(5), Label: "m"},
	{ID: 17, Grp: nil, Score: i32(10), Label: "n"},
	{ID: 18, Grp: i32(3), Score: i32(7), Label: "o"},
	{ID: 19, Grp: i32(5), Score: nil, Label: "p"},
	{ID: 20, Grp: i32(5), Score: i32(1), Label: "q"},
	{ID: 21, Grp: i32(1), Score: nil, Label: "r"},
	{ID: 22, Grp: nil, Score: i32(5), Label: "s"},
	{ID: 23, Grp: i32(4), Score: i32(5), Label: "t"},
	{ID: 24, Grp: i32(2), Score: i32(9), Label: "u"},
}

func TestMain(m *testing.M) {
	db, err := sql.Open("postgres", dbconfig.PostgresConnectString)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open postgres: %v\n", err)
		os.Exit(1)
	}
	if err := db.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect to postgres at %s: %v\n", dbconfig.PostgresConnectString, err)
		os.Exit(1)
	}

	if err := setupSeekTable(db); err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup seek table: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	_, _ = db.Exec("DROP TABLE IF EXISTS seek_page_items")
	_ = db.Close()
	os.Exit(code)
}

func setupSeekTable(db *sql.DB) error {
	if _, err := db.Exec("DROP TABLE IF EXISTS seek_page_items"); err != nil {
		return err
	}
	if _, err := db.Exec(`
CREATE TABLE seek_page_items (
	id    integer PRIMARY KEY,
	grp   integer,
	score integer,
	label text NOT NULL
)`); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, row := range seedRows {
		var grp, score interface{}
		if row.Grp != nil {
			grp = *row.Grp
		}
		if row.Score != nil {
			score = *row.Score
		}
		if _, err := tx.Exec(
			"INSERT INTO seek_page_items(id, grp, score, label) VALUES ($1, $2, $3, $4)",
			row.ID, grp, score, row.Label); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// seekDim is one dimension of a test sort specification.
type seekDim struct {
	expr        postgres.Expression
	asc         bool
	nulls       postgres.SeekNullsOrder
	isNull      func(seekItem) bool
	cursorValue func(seekItem) interface{}
	// cmp compares two non-NULL values; negative when a sorts before b on the raw values.
	cmp func(a, b seekItem) int
}

type seekSpec struct {
	name  string
	dims  []seekDim
	idAsc bool
}

func intDim(expr postgres.Expression, asc bool, nulls postgres.SeekNullsOrder, get func(seekItem) *int32) seekDim {
	return seekDim{
		expr:   expr,
		asc:    asc,
		nulls:  nulls,
		isNull: func(r seekItem) bool { return get(r) == nil },
		cursorValue: func(r seekItem) interface{} {
			if v := get(r); v != nil {
				return *v
			}
			return nil
		},
		cmp: func(a, b seekItem) int {
			return int(*get(a)) - int(*get(b))
		},
	}
}

func stringDim(expr postgres.Expression, asc bool, nulls postgres.SeekNullsOrder, get func(seekItem) string) seekDim {
	return seekDim{
		expr:        expr,
		asc:         asc,
		nulls:       nulls,
		isNull:      func(seekItem) bool { return false },
		cursorValue: func(r seekItem) interface{} { return get(r) },
		cmp:         func(a, b seekItem) int { return strings.Compare(get(a), get(b)) },
	}
}

// allSpecs covers every ASC/DESC x NULLS FIRST/LAST combination, mixed directions,
// single-dimension NULL group crossing and a non-column native expression.
func allSpecs() []seekSpec {
	return []seekSpec{
		{
			name: "grp ASC NULLS LAST, score ASC NULLS LAST, id ASC",
			dims: []seekDim{
				intDim(colGrp, true, postgres.SeekNullsLast, func(r seekItem) *int32 { return r.Grp }),
				intDim(colScore, true, postgres.SeekNullsLast, func(r seekItem) *int32 { return r.Score }),
			},
			idAsc: true,
		},
		{
			name: "grp DESC NULLS FIRST, score ASC NULLS LAST, id DESC",
			dims: []seekDim{
				intDim(colGrp, false, postgres.SeekNullsFirst, func(r seekItem) *int32 { return r.Grp }),
				intDim(colScore, true, postgres.SeekNullsLast, func(r seekItem) *int32 { return r.Score }),
			},
			idAsc: false,
		},
		{
			name: "grp ASC NULLS FIRST, score DESC NULLS FIRST, id ASC",
			dims: []seekDim{
				intDim(colGrp, true, postgres.SeekNullsFirst, func(r seekItem) *int32 { return r.Grp }),
				intDim(colScore, false, postgres.SeekNullsFirst, func(r seekItem) *int32 { return r.Score }),
			},
			idAsc: true,
		},
		{
			name: "grp DESC NULLS LAST, score DESC NULLS LAST, id DESC",
			dims: []seekDim{
				intDim(colGrp, false, postgres.SeekNullsLast, func(r seekItem) *int32 { return r.Grp }),
				intDim(colScore, false, postgres.SeekNullsLast, func(r seekItem) *int32 { return r.Score }),
			},
			idAsc: false,
		},
		{
			name:  "score ASC NULLS FIRST, id ASC (single nullable dimension)",
			dims:  []seekDim{intDim(colScore, true, postgres.SeekNullsFirst, func(r seekItem) *int32 { return r.Score })},
			idAsc: true,
		},
		{
			name:  "label DESC NULLS LAST, id ASC (text dimension)",
			dims:  []seekDim{stringDim(colLabel, false, postgres.SeekNullsLast, func(r seekItem) string { return r.Label })},
			idAsc: true,
		},
		{
			name: "LOWER(label) ASC NULLS LAST, id ASC (native non-column expression)",
			dims: []seekDim{
				stringDim(postgres.LOWER(colLabel), true, postgres.SeekNullsLast,
					func(r seekItem) string { return strings.ToLower(r.Label) }),
			},
			idAsc: true,
		},
	}
}

// compareRows mirrors the SQL lexicographic ordering declared by the spec, including the
// explicit NULL position and the unique id tiebreaker.
func compareRows(spec seekSpec, a, b seekItem) int {
	for _, dim := range spec.dims {
		aNil, bNil := dim.isNull(a), dim.isNull(b)

		if aNil || bNil {
			if aNil && bNil {
				continue
			}
			// NULL first means the NULL row sorts before the non-NULL row.
			if (aNil && dim.nulls == postgres.SeekNullsFirst) || (bNil && dim.nulls == postgres.SeekNullsLast) {
				return -1
			}
			return 1
		}

		if c := dim.cmp(a, b); c != 0 {
			if !dim.asc {
				c = -c
			}
			return c
		}
	}

	if a.ID == b.ID {
		return 0
	}
	if spec.idAsc {
		if a.ID < b.ID {
			return -1
		}
		return 1
	}
	if a.ID > b.ID {
		return -1
	}
	return 1
}

func expectedOrder(rows []seekItem, spec seekSpec, include func(seekItem) bool) []seekItem {
	out := make([]seekItem, 0, len(rows))
	for _, row := range rows {
		if include == nil || include(row) {
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return compareRows(spec, out[i], out[j]) < 0
	})
	return out
}

func (spec seekSpec) sorts() []postgres.SeekSort {
	sorts := make([]postgres.SeekSort, len(spec.dims))
	for i, dim := range spec.dims {
		sorts[i] = postgres.SeekSort{Expression: dim.expr, Ascending: dim.asc, Nulls: dim.nulls}
	}
	return sorts
}

func (spec seekSpec) cursor(row seekItem) []interface{} {
	cursor := make([]interface{}, len(spec.dims))
	for i, dim := range spec.dims {
		cursor[i] = dim.cursorValue(row)
	}
	return cursor
}

func baseSelect(filtered bool) postgres.SelectStatement {
	stmt := postgres.SELECT(colID, colGrp, colScore, colLabel).FROM(seekItems)
	if filtered {
		stmt = stmt.WHERE(colID.LT(postgres.Int(18)))
	}
	return stmt
}

func newPageRequest(direction postgres.SeekDirection, spec seekSpec, row *seekItem, pageSize int64) postgres.SeekPageRequest {
	req := postgres.SeekPageRequest{
		Direction: direction,
		Sorts:     spec.sorts(),
		UniqueKey: postgres.SeekUniqueKey{Expression: colID, Ascending: spec.idAsc},
		Limit:     pageSize,
	}
	if row != nil {
		req.Cursor = spec.cursor(*row)
		req.UniqueKey.Value = row.ID
	}
	return req
}

// pageForward walks every next page with SeekAfter and returns the concatenated row ids.
func pageForward(t *testing.T, db *sql.DB, spec seekSpec, pageSize int, base postgres.SelectStatement) []int32 {
	t.Helper()

	var (
		gotIDs []int32
		cursor *seekItem
	)

	for {
		page, err := postgres.NewSeekPage(newPageRequest(postgres.SeekAfter, spec, cursor, int64(pageSize)))
		require.NoError(t, err)
		require.Equal(t, postgres.SeekCanonicalOrder, page.ResultOrder)

		var rows []seekItem
		require.NoError(t, page.ApplyTo(base).Query(db, &rows))
		require.LessOrEqual(t, len(rows), pageSize)
		if len(rows) == 0 {
			break
		}

		gotIDs = append(gotIDs, idsOf(rows)...)
		last := rows[len(rows)-1]
		cursor = &last

		if len(rows) < pageSize {
			// The request after the last row must return no rows.
			emptyPage, err := postgres.NewSeekPage(newPageRequest(postgres.SeekAfter, spec, cursor, int64(pageSize)))
			require.NoError(t, err)
			var emptyRows []seekItem
			require.NoError(t, emptyPage.ApplyTo(base).Query(db, &emptyRows))
			require.Empty(t, emptyRows)
			break
		}
	}

	return gotIDs
}

func idsOf(rows []seekItem) []int32 {
	ids := make([]int32, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	return ids
}

func TestSeekPageForwardMatchesFullSort(t *testing.T) {
	db := openDB(t)

	for _, spec := range allSpecs() {
		for _, pageSize := range []int{2, 5} {
			t.Run(fmt.Sprintf("%s [page %d]", spec.name, pageSize), func(t *testing.T) {
				expected := expectedOrder(seedRows, spec, nil)
				gotIDs := pageForward(t, db, spec, pageSize, baseSelect(false))

				require.Equal(t, idsOf(expected), gotIDs, "forward paging must reproduce the full sorted order")
				requireUniqueAndComplete(t, gotIDs, len(expected))
			})
		}
	}
}

func TestSeekPageBackwardMatchesReversedFullSort(t *testing.T) {
	db := openDB(t)

	for _, spec := range allSpecs() {
		for _, pageSize := range []int{2, 5} {
			t.Run(fmt.Sprintf("%s [page %d]", spec.name, pageSize), func(t *testing.T) {
				expected := expectedOrder(seedRows, spec, nil)

				// Start from the last row of the canonical order and walk SeekBefore pages
				// until the result set is exhausted.
				cursor := expected[len(expected)-1]
				var nearestFirst []int32
				visited := map[int32]bool{}

				for {
					page, err := postgres.NewSeekPage(newPageRequest(postgres.SeekBefore, spec, &cursor, int64(pageSize)))
					require.NoError(t, err)
					require.Equal(t, postgres.SeekReverseOrder, page.ResultOrder)

					var rows []seekItem
					require.NoError(t, page.ApplyTo(baseSelect(false)).Query(db, &rows))
					if len(rows) == 0 {
						break
					}
					require.LessOrEqual(t, len(rows), pageSize)

					// The first row is always the immediate predecessor of the cursor row:
					// this is the nearest previous page, never the farthest one.
					require.Equal(t, predecessorID(expected, cursor.ID), rows[0].ID)

					for _, row := range rows {
						require.False(t, visited[row.ID], "duplicate row %d during backward paging", row.ID)
						visited[row.ID] = true
						nearestFirst = append(nearestFirst, row.ID)
					}

					// Continue from the farthest row of this reversed page.
					cursor = rows[len(rows)-1]
				}

				// Pages arrive in reverse canonical order; reversing the collected ids must
				// reproduce the canonical prefix before the anchor.
				canonical := reverseInt32(nearestFirst)
				expectedPrefix := idsOf(expected[:len(expected)-1])
				require.Equal(t, expectedPrefix, canonical)
				require.Len(t, visited, len(expected)-1)
			})
		}
	}
}

func TestSeekPageStrictBoundaryExcludesCursorRow(t *testing.T) {
	db := openDB(t)

	for _, spec := range allSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			expected := expectedOrder(seedRows, spec, nil)

			for _, index := range []int{3, 10, len(expected) - 2} {
				cursorRow := expected[index]

				afterPage, err := postgres.NewSeekPage(newPageRequest(postgres.SeekAfter, spec, &cursorRow, 1))
				require.NoError(t, err)
				var afterRows []seekItem
				require.NoError(t, afterPage.ApplyTo(baseSelect(false)).Query(db, &afterRows))
				require.Len(t, afterRows, 1)
				require.Equal(t, expected[index+1].ID, afterRows[0].ID, "after must start at the immediate successor")
				require.NotEqual(t, cursorRow.ID, afterRows[0].ID)

				beforePage, err := postgres.NewSeekPage(newPageRequest(postgres.SeekBefore, spec, &cursorRow, 1))
				require.NoError(t, err)
				var beforeRows []seekItem
				require.NoError(t, beforePage.ApplyTo(baseSelect(false)).Query(db, &beforeRows))
				require.Len(t, beforeRows, 1)
				require.Equal(t, expected[index-1].ID, beforeRows[0].ID, "before must start at the immediate predecessor")
				require.NotEqual(t, cursorRow.ID, beforeRows[0].ID)
			}
		})
	}
}

func TestSeekPageBidirectionalWalkHasNoGaps(t *testing.T) {
	db := openDB(t)
	spec := allSpecs()[1] // mixed DESC/ASC with NULLS FIRST/LAST
	expected := expectedOrder(seedRows, spec, nil)
	pageSize := 3

	// Forward: first two pages.
	var (
		seen   []int32
		cursor *seekItem
	)
	for i := 0; i < 2; i++ {
		page, err := postgres.NewSeekPage(newPageRequest(postgres.SeekAfter, spec, cursor, int64(pageSize)))
		require.NoError(t, err)
		var rows []seekItem
		require.NoError(t, page.ApplyTo(baseSelect(false)).Query(db, &rows))
		require.Len(t, rows, pageSize)
		seen = append(seen, idsOf(rows)...)
		last := rows[len(rows)-1]
		cursor = &last
	}

	// Backward from the last seen row: must enumerate exactly the rows already seen,
	// nearest first, without duplicates or missing rows.
	var back []int32
	for {
		page, err := postgres.NewSeekPage(newPageRequest(postgres.SeekBefore, spec, cursor, int64(pageSize)))
		require.NoError(t, err)
		var rows []seekItem
		require.NoError(t, page.ApplyTo(baseSelect(false)).Query(db, &rows))
		if len(rows) == 0 {
			break
		}
		back = append(back, idsOf(rows)...)
		edge := rows[len(rows)-1]
		cursor = &edge
	}

	// The anchor row is excluded by the strict predicate, so the backward walk returns the
	// already seen predecessors (all seen rows except the anchor), in reverse order.
	require.Equal(t, reverseInt32(seen[:len(seen)-1]), back,
		"backward walk must return the seen predecessors in reverse")

	// A full forward walk still reproduces the complete canonical order without gaps.
	full := pageForward(t, db, spec, pageSize, baseSelect(false))
	require.Equal(t, idsOf(expected), full)
}

func TestSeekPageForwardWithFilter(t *testing.T) {
	db := openDB(t)
	filter := func(row seekItem) bool { return row.ID < 18 }

	for _, spec := range allSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			expected := expectedOrder(seedRows, spec, filter)
			gotIDs := pageForward(t, db, spec, 3, baseSelect(true))
			require.Equal(t, idsOf(expected), gotIDs)
			requireUniqueAndComplete(t, gotIDs, len(expected))
		})
	}

	// The filter owns the first parameter; seek and LIMIT parameters must continue its numbering.
	spec := allSpecs()[0]
	row := expectedOrder(seedRows, spec, filter)[4]
	page, err := postgres.NewSeekPage(newPageRequest(postgres.SeekAfter, spec, &row, 3))
	require.NoError(t, err)
	query, args := page.ApplyTo(baseSelect(true)).Sql()
	require.Contains(t, query, "seek_item.id < $1")
	for i := 2; i <= len(args); i++ {
		require.Contains(t, query, fmt.Sprintf("$%d", i))
	}
	require.NotContains(t, query, "OFFSET")
}

func TestSeekPageAllNullCursorNullsLastReturnsEmptyPage(t *testing.T) {
	db := openDB(t)

	// Every dimension is NULLS LAST with a NULL cursor (including the unique key value).
	// A NULL value is the top of each such dimension, so no row can be strictly after it and
	// the generated always-false predicate must execute successfully and return no rows.
	nilGrpRow := seekItem{ID: 2, Grp: nil, Score: nil, Label: "b"}
	page, err := postgres.NewSeekPage(postgres.SeekPageRequest{
		Direction: postgres.SeekAfter,
		Sorts: []postgres.SeekSort{
			{Expression: colGrp, Ascending: true, Nulls: postgres.SeekNullsLast},
			{Expression: colScore, Ascending: true, Nulls: postgres.SeekNullsLast},
		},
		UniqueKey: postgres.SeekUniqueKey{Expression: colID, Ascending: true, Value: nil},
		Cursor:    []interface{}{nil, nil},
		Limit:     10,
	})
	require.NoError(t, err)

	var rows []seekItem
	require.NoError(t, page.ApplyTo(baseSelect(false)).Query(db, &rows))
	require.Empty(t, rows)

	// The same request in SeekBefore flips to NULLS FIRST and reaches the NULL group.
	beforePage, err := postgres.NewSeekPage(postgres.SeekPageRequest{
		Direction: postgres.SeekBefore,
		Sorts: []postgres.SeekSort{
			{Expression: colGrp, Ascending: true, Nulls: postgres.SeekNullsLast},
			{Expression: colScore, Ascending: true, Nulls: postgres.SeekNullsLast},
		},
		UniqueKey: postgres.SeekUniqueKey{Expression: colID, Ascending: true, Value: nilGrpRow.ID},
		Cursor:    []interface{}{nil, nil},
		Limit:     10,
	})
	require.NoError(t, err)
	require.Equal(t, postgres.SeekReverseOrder, beforePage.ResultOrder)

	var beforeRows []seekItem
	require.NoError(t, beforePage.ApplyTo(baseSelect(false)).Query(db, &beforeRows))
	require.NotEmpty(t, beforeRows)
	for _, row := range beforeRows {
		require.NotEqual(t, nilGrpRow.ID, row.ID, "cursor row must not be repeated")
	}
}

func TestSeekPageGeneratedSqlHasNoOffsetOrSentinels(t *testing.T) {
	spec := allSpecs()[0]
	row := seedRows[1] // contains NULL grp and NULL score

	page, err := postgres.NewSeekPage(newPageRequest(postgres.SeekAfter, spec, &row, 4))
	require.NoError(t, err)

	query, _ := page.ApplyTo(baseSelect(false)).Sql()
	require.NotContains(t, query, "OFFSET")
	require.NotContains(t, query, "COALESCE")
	// NULL cursor values travel as NULL-safe equality against bound parameters, not as
	// inline literals or sentinel values.
	require.Contains(t, query, "IS NOT DISTINCT FROM")
	require.Contains(t, query, "NULLS LAST")
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dbconfig.PostgresConnectString)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func requireUniqueAndComplete(t *testing.T, ids []int32, expectedCount int) {
	t.Helper()
	seen := map[int32]bool{}
	for _, id := range ids {
		require.False(t, seen[id], "duplicate id %d", id)
		seen[id] = true
	}
	require.Len(t, ids, expectedCount)
}

func predecessorID(rows []seekItem, id int32) int32 {
	for i, row := range rows {
		if row.ID == id {
			if i == 0 {
				return -1
			}
			return rows[i-1].ID
		}
	}
	return -1
}

func reverseInt32(values []int32) []int32 {
	out := make([]int32, len(values))
	for i, v := range values {
		out[len(values)-1-i] = v
	}
	return out
}
