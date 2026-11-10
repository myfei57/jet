package postgres

import (
	"strings"
	"testing"

	"github.com/go-jet/jet/v2/internal/jet"
	"github.com/stretchr/testify/require"
)

// seekTestTable is an ad-hoc table definition; generated models are not required to test the
// seek page builder.
var (
	seekCol1 = IntegerColumn("col1")
	seekCol2 = StringColumn("col2")
	seekID   = IntegerColumn("id")

	seekTable = NewTable("", "seek_demo", "", seekCol1, seekCol2, seekID)
)

func seekBaseStatement() SelectStatement {
	return SELECT(seekCol1, seekCol2, seekID).FROM(seekTable)
}

func TestSeekPageValidation(t *testing.T) {
	validSorts := []SeekSort{{Expression: seekCol1, Ascending: true, Nulls: SeekNullsLast}}
	validKey := SeekUniqueKey{Expression: seekID, Ascending: true, Value: int64(1)}

	t.Run("empty sort list", func(t *testing.T) {
		_, err := NewSeekPage(SeekPageRequest{
			Sorts:     nil,
			UniqueKey: validKey,
			Cursor:    []interface{}{},
			Limit:     10,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "at least one sort expression")
	})

	t.Run("nil unique key", func(t *testing.T) {
		_, err := NewSeekPage(SeekPageRequest{
			Sorts:  validSorts,
			Cursor: []interface{}{int64(1)},
			Limit:  10,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "unique key expression")
	})

	t.Run("illegal limit", func(t *testing.T) {
		for _, limit := range []int64{0, -1, -100} {
			_, err := NewSeekPage(SeekPageRequest{
				Sorts:     validSorts,
				UniqueKey: validKey,
				Cursor:    []interface{}{int64(1)},
				Limit:     limit,
			})
			require.Error(t, err)
			require.Contains(t, err.Error(), "limit must be strictly positive")
		}
	})

	t.Run("cursor dimension mismatch", func(t *testing.T) {
		_, err := NewSeekPage(SeekPageRequest{
			Sorts: []SeekSort{
				{Expression: seekCol1, Ascending: true, Nulls: SeekNullsLast},
				{Expression: seekCol2, Ascending: false, Nulls: SeekNullsFirst},
			},
			UniqueKey: validKey,
			Cursor:    []interface{}{int64(1)}, // only one value for two dimensions
			Limit:     10,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "cursor dimension mismatch")
	})

	t.Run("nil sort expression", func(t *testing.T) {
		_, err := NewSeekPage(SeekPageRequest{
			Sorts:     []SeekSort{{Expression: nil, Ascending: true, Nulls: SeekNullsLast}},
			UniqueKey: validKey,
			Cursor:    []interface{}{int64(1)},
			Limit:     10,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "sort expression at index 0 is nil")
	})

	t.Run("invalid direction", func(t *testing.T) {
		_, err := NewSeekPage(SeekPageRequest{
			Direction: SeekDirection(42),
			Sorts:     validSorts,
			UniqueKey: validKey,
			Cursor:    []interface{}{int64(1)},
			Limit:     10,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid direction")
	})

	t.Run("raw expression rejected by default", func(t *testing.T) {
		_, err := NewSeekPage(SeekPageRequest{
			Sorts:     []SeekSort{{Expression: RawInt("abs(col1)"), Ascending: true, Nulls: SeekNullsLast}},
			UniqueKey: validKey,
			Cursor:    []interface{}{int64(1)},
			Limit:     10,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "Raw expression")
		require.Contains(t, err.Error(), "AllowRawExpressions")
	})

	t.Run("raw expression accepted with capability contract", func(t *testing.T) {
		page, err := NewSeekPage(SeekPageRequest{
			Sorts:               []SeekSort{{Expression: RawInt("abs(col1)"), Ascending: true, Nulls: SeekNullsLast}},
			UniqueKey:           validKey,
			Cursor:              []interface{}{int64(1)},
			Limit:               10,
			AllowRawExpressions: true,
		})
		require.NoError(t, err)
		require.NotNil(t, page.Predicate)
	})

	t.Run("native and column expressions accepted", func(t *testing.T) {
		for _, expr := range []Expression{
			seekCol1,
			LOWER(seekCol2),
			COALESCE(seekCol1, Int(0)),
		} {
			_, err := NewSeekPage(SeekPageRequest{
				Sorts:     []SeekSort{{Expression: expr, Ascending: true, Nulls: SeekNullsLast}},
				UniqueKey: validKey,
				Cursor:    []interface{}{int64(1)},
				Limit:     10,
			})
			require.NoError(t, err)
		}
	})

	t.Run("raw detection helper", func(t *testing.T) {
		require.True(t, jet.IsRawExpression(Raw("length(col2)")))
		require.True(t, jet.IsRawExpression(RawString("lower(col2)")))
		require.False(t, jet.IsRawExpression(seekCol1))
		require.False(t, jet.IsRawExpression(LOWER(seekCol2)))
		require.False(t, jet.IsRawExpression(jet.Literal(1)))
		require.False(t, jet.IsRawExpression(nil))
	})
}

func TestSeekPageFirstPageHasNoPredicate(t *testing.T) {
	page, err := NewSeekPage(SeekPageRequest{
		Direction: SeekAfter,
		Sorts:     []SeekSort{{Expression: seekCol1, Ascending: true, Nulls: SeekNullsLast}},
		UniqueKey: SeekUniqueKey{Expression: seekID, Ascending: true},
		Limit:     5,
	})
	require.NoError(t, err)
	require.Nil(t, page.Predicate)
	require.Equal(t, SeekCanonicalOrder, page.ResultOrder)
	require.Equal(t, int64(5), page.Limit)

	assertStatementSql(t, page.ApplyTo(seekBaseStatement()), `
SELECT seek_demo.col1 AS "seek_demo.col1",
     seek_demo.col2 AS "seek_demo.col2",
     seek_demo.id AS "seek_demo.id"
FROM seek_demo
ORDER BY seek_demo.col1 ASC NULLS LAST, seek_demo.id ASC NULLS LAST
LIMIT $1;
`, int64(5))
}

func TestSeekPageAfterSingleDimension(t *testing.T) {
	page, err := NewSeekPage(SeekPageRequest{
		Direction: SeekAfter,
		Sorts:     []SeekSort{{Expression: seekCol1, Ascending: true, Nulls: SeekNullsLast}},
		UniqueKey: SeekUniqueKey{Expression: seekID, Ascending: true, Value: int64(7)},
		Cursor:    []interface{}{int64(5)},
		Limit:     3,
	})
	require.NoError(t, err)
	require.Equal(t, SeekCanonicalOrder, page.ResultOrder)

	// Lexicographic expansion:
	//   branch 0: col1 strictly after cursor (NULL row counts because of NULLS LAST)
	//   branch 1: col1 NULL-safe equal and id strictly after its cursor
	assertStatementSql(t, page.ApplyTo(seekBaseStatement()), `
SELECT seek_demo.col1 AS "seek_demo.col1",
     seek_demo.col2 AS "seek_demo.col2",
     seek_demo.id AS "seek_demo.id"
FROM seek_demo
WHERE (
          (
                     (seek_demo.col1 IS NULL)
                         OR (seek_demo.col1 > $1)
                 )
              OR (
                     (seek_demo.col1 IS NOT DISTINCT FROM $2)
                         AND (
                                 (seek_demo.id IS NULL)
                                     OR (seek_demo.id > $3)
                             )
                 )
      )
ORDER BY seek_demo.col1 ASC NULLS LAST, seek_demo.id ASC NULLS LAST
LIMIT $4;
`, int64(5), int64(5), int64(7), int64(3))
}

func TestSeekPageNullCursorNullsFirst(t *testing.T) {
	// NULL cursor with NULLS FIRST: every non-NULL row is strictly after the NULL group;
	// another NULL row is an equal prefix resolved by the unique key.
	page, err := NewSeekPage(SeekPageRequest{
		Direction: SeekAfter,
		Sorts:     []SeekSort{{Expression: seekCol1, Ascending: true, Nulls: SeekNullsFirst}},
		UniqueKey: SeekUniqueKey{Expression: seekID, Ascending: true, Value: int64(7)},
		Cursor:    []interface{}{nil},
		Limit:     3,
	})
	require.NoError(t, err)

	assertStatementSql(t, page.ApplyTo(seekBaseStatement()), `
SELECT seek_demo.col1 AS "seek_demo.col1",
     seek_demo.col2 AS "seek_demo.col2",
     seek_demo.id AS "seek_demo.id"
FROM seek_demo
WHERE (
          (seek_demo.col1 IS NOT NULL)
              OR (
                     (seek_demo.col1 IS NOT DISTINCT FROM $1)
                         AND (
                                 (seek_demo.id IS NULL)
                                     OR (seek_demo.id > $2)
                             )
                 )
      )
ORDER BY seek_demo.col1 ASC NULLS FIRST, seek_demo.id ASC NULLS LAST
LIMIT $3;
`, nil, int64(7), int64(3))
}

func TestSeekPageNullCursorNullsLastIsImpossible(t *testing.T) {
	// NULL cursor under NULLS LAST is the top of the dimension, so its strict branch is omitted.
	// With the unique key cursor also unset, no branch remains and an always-false predicate is
	// emitted, yielding an empty page instead of an invalid query.
	page, err := NewSeekPage(SeekPageRequest{
		Direction: SeekAfter,
		Sorts:     []SeekSort{{Expression: seekCol1, Ascending: true, Nulls: SeekNullsLast}},
		UniqueKey: SeekUniqueKey{Expression: seekID, Ascending: true, Value: nil},
		Cursor:    []interface{}{nil},
		Limit:     3,
	})
	require.NoError(t, err)
	require.NotNil(t, page.Predicate)

	query, args := page.ApplyTo(seekBaseStatement()).Sql()
	require.Contains(t, query, "WHERE $1")
	require.Equal(t, []interface{}{false, int64(3)}, args)
}

func TestSeekPageBeforeReversesAllDirectionsAndNulls(t *testing.T) {
	page, err := NewSeekPage(SeekPageRequest{
		Direction: SeekBefore,
		Sorts: []SeekSort{
			{Expression: seekCol1, Ascending: true, Nulls: SeekNullsLast},
			{Expression: seekCol2, Ascending: false, Nulls: SeekNullsFirst},
		},
		UniqueKey: SeekUniqueKey{Expression: seekID, Ascending: true, Value: int64(10)},
		Cursor:    []interface{}{nil, "b"},
		Limit:     3,
	})
	require.NoError(t, err)
	require.Equal(t, SeekReverseOrder, page.ResultOrder)

	query, args := page.ApplyTo(seekBaseStatement()).Sql()

	// Every ASC/DESC and NULLS FIRST/LAST pair must be reversed, including the unique key.
	require.Contains(t, query, "ORDER BY seek_demo.col1 DESC NULLS FIRST, seek_demo.col2 ASC NULLS LAST, seek_demo.id DESC NULLS FIRST")
	require.Contains(t, query, "seek_demo.col1 IS NOT NULL") // NULL cursor, flipped NULLS FIRST
	require.Contains(t, query, "(seek_demo.col2 IS NULL)")   // non-NULL cursor, flipped NULLS LAST
	require.NotContains(t, query, "OFFSET")
	require.Equal(t, []interface{}{nil, "b", nil, "b", int64(10), int64(3)}, args)
}

func TestSeekPageSubqueryParameterNumbering(t *testing.T) {
	// The base query already owns $1 through a sub-query; seek parameters must continue the
	// numbering on the same SQLBuilder instead of restarting or colliding.
	subQuery := SELECT(seekID).FROM(seekTable).WHERE(seekCol1.LT(Int(42)))
	base := seekBaseStatement().WHERE(seekID.IN(subQuery))

	page, err := NewSeekPage(SeekPageRequest{
		Direction: SeekAfter,
		Sorts:     []SeekSort{{Expression: seekCol1, Ascending: true, Nulls: SeekNullsLast}},
		UniqueKey: SeekUniqueKey{Expression: seekID, Ascending: true, Value: int64(7)},
		Cursor:    []interface{}{int64(5)},
		Limit:     3,
	})
	require.NoError(t, err)

	query, args := page.ApplyTo(base).Sql()
	require.Contains(t, query, "WHERE seek_demo.col1 < $1")
	require.Contains(t, query, "seek_demo.col1 > $2")
	require.Contains(t, query, "seek_demo.col1 IS NOT DISTINCT FROM $3")
	require.Contains(t, query, "seek_demo.id > $4")
	require.Contains(t, query, "LIMIT $5")
	require.Equal(t, []interface{}{int64(42), int64(5), int64(5), int64(7), int64(3)}, args)
}

func TestSeekPageCombinesExistingWhereAndKeepsOriginalUnchanged(t *testing.T) {
	base := seekBaseStatement().WHERE(seekCol1.GT(Int(99)))
	originalSQL, originalArgs := base.Sql()

	page, err := NewSeekPage(SeekPageRequest{
		Direction: SeekAfter,
		Sorts:     []SeekSort{{Expression: seekCol2, Ascending: false, Nulls: SeekNullsFirst}},
		UniqueKey: SeekUniqueKey{Expression: seekID, Ascending: true, Value: int64(10)},
		Cursor:    []interface{}{"b"},
		Limit:     3,
	})
	require.NoError(t, err)

	applied := page.ApplyTo(base)
	appliedSQL, appliedArgs := applied.Sql()

	// The input statement is not rewritten.
	unchangedSQL, unchangedArgs := base.Sql()
	require.Equal(t, originalSQL, unchangedSQL)
	require.Equal(t, originalArgs, unchangedArgs)

	require.Contains(t, appliedSQL, "seek_demo.col1 > $1")
	require.Contains(t, appliedSQL, "AND")
	require.Contains(t, appliedSQL, "seek_demo.col2 < $2")
	require.Contains(t, appliedSQL, "seek_demo.col2 IS NOT DISTINCT FROM $3")
	require.NotContains(t, appliedSQL, "OFFSET")
	require.Equal(t, []interface{}{int64(99), "b", "b", int64(10), int64(3)}, appliedArgs)
}

func TestSeekPageNeverEmitsOffset(t *testing.T) {
	for _, direction := range []SeekDirection{SeekAfter, SeekBefore} {
		page, err := NewSeekPage(SeekPageRequest{
			Direction: direction,
			Sorts: []SeekSort{
				{Expression: seekCol1, Ascending: true, Nulls: SeekNullsFirst},
				{Expression: seekCol2, Ascending: false, Nulls: SeekNullsLast},
			},
			UniqueKey: SeekUniqueKey{Expression: seekID, Ascending: true, Value: int64(10)},
			Cursor:    []interface{}{int64(1), "x"},
			Limit:     25,
		})
		require.NoError(t, err)

		query, _ := page.ApplyTo(seekBaseStatement()).Sql()
		require.False(t, strings.Contains(query, "OFFSET"))
	}
}

func TestSeekPageExplicitNullsAndDirectionsInOrderBy(t *testing.T) {
	page, err := NewSeekPage(SeekPageRequest{
		Direction: SeekAfter,
		Sorts: []SeekSort{
			{Expression: seekCol1, Ascending: false, Nulls: SeekNullsFirst},
			{Expression: seekCol2, Ascending: true, Nulls: SeekNullsLast},
		},
		UniqueKey: SeekUniqueKey{Expression: seekID, Ascending: false},
		Cursor:    []interface{}{int64(1), "x"},
		Limit:     4,
	})
	require.NoError(t, err)

	require.Len(t, page.OrderBy, 3)
	query, _ := page.ApplyTo(seekBaseStatement()).Sql()
	require.Contains(t, query,
		"ORDER BY seek_demo.col1 DESC NULLS FIRST, seek_demo.col2 ASC NULLS LAST, seek_demo.id DESC NULLS LAST")
}

func TestSeekPageApplyToRejectsNonSelectStatement(t *testing.T) {
	page, err := NewSeekPage(SeekPageRequest{
		Sorts:     []SeekSort{{Expression: seekCol1, Ascending: true, Nulls: SeekNullsLast}},
		UniqueKey: SeekUniqueKey{Expression: seekID},
		Limit:     1,
	})
	require.NoError(t, err)

	assertPanicErr(t, func() {
		var stmt SelectStatement // nil statement is not a concrete select statement
		page.ApplyTo(stmt)
	}, "jet: seek page can only be applied to a postgres.SelectStatement")
}
