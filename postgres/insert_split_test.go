package postgres

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-jet/jet/v2/internal/jet"
	"github.com/stretchr/testify/require"
)

// assertShardSql verifies that a shard renders exactly ParamCount arguments and numbers its
// placeholders contiguously from $1 to $N.
func assertShardSql(t *testing.T, shard InsertShard) (string, []interface{}) {
	t.Helper()

	query, args := shard.Statement.Sql()

	require.Equal(t, shard.ParamCount, len(args), "shard rows %d-%d argument count", shard.RowStart, shard.RowEnd)

	matches := placeholderRegexp.FindAllStringSubmatch(query, -1)
	require.Len(t, matches, len(args), "shard rows %d-%d placeholder count", shard.RowStart, shard.RowEnd)

	for index, match := range matches {
		require.Equal(t, fmt.Sprintf("$%d", index+1), match[0],
			"shard rows %d-%d placeholders must start at $1 and be contiguous", shard.RowStart, shard.RowEnd)
	}

	return query, args
}

func assertRanges(t *testing.T, shards []InsertShard, expected [][2]int) {
	t.Helper()

	require.Len(t, shards, len(expected))

	for index, shard := range shards {
		require.Equal(t, expected[index][0], shard.RowStart, "shard %d RowStart", index)
		require.Equal(t, expected[index][1], shard.RowEnd, "shard %d RowEnd", index)
		require.Equal(t, shard.RowEnd-shard.RowStart > 0, true)
	}

	// Rows must be covered exactly once, in original order.
	require.Equal(t, 0, shards[0].RowStart)
	for index := 1; index < len(shards); index++ {
		require.Equal(t, shards[index-1].RowEnd, shards[index].RowStart)
	}
	require.Equal(t, expected[len(expected)-1][1], shards[len(shards)-1].RowEnd)
}

func TestSplitInsertExactLimitAndRanges(t *testing.T) {
	stmt := table1.INSERT(table1Col1, table1ColFloat).
		VALUES(1, 1.1).
		VALUES(2, 2.2).
		VALUES(3, 3.3).
		VALUES(4, 4.4).
		VALUES(5, 5.5)

	// The limit is exactly equal to the total parameter count: everything fits in one shard.
	shards, err := SplitInsertByParamBudget(stmt, 10, AcceptShardingSemantics())
	require.NoError(t, err)
	assertRanges(t, shards, [][2]int{{0, 5}})
	require.Equal(t, 10, shards[0].ParamCount)
	assertShardSql(t, shards[0])

	// Two rows per shard, last shard holds the remaining row.
	shards, err = SplitInsertByParamBudget(stmt, 4, AcceptShardingSemantics())
	require.NoError(t, err)
	assertRanges(t, shards, [][2]int{{0, 2}, {2, 4}, {4, 5}})
	require.Equal(t, []int{4, 4, 2}, []int{shards[0].ParamCount, shards[1].ParamCount, shards[2].ParamCount})

	allArgs := make([]interface{}, 0)
	for _, shard := range shards {
		query, args := assertShardSql(t, shard)
		require.True(t, strings.HasPrefix(query, "\nINSERT INTO db.table1 (col1, col_float)"))
		allArgs = append(allArgs, args...)
	}

	// Original row order and values are preserved across the shards.
	require.Equal(t, []interface{}{1, 1.1, 2, 2.2, 3, 3.3, 4, 4.4, 5, 5.5}, allArgs)

	// A limit that fits only one row at a time: each row becomes its own shard.
	shards, err = SplitInsertByParamBudget(stmt, 3, AcceptShardingSemantics())
	require.NoError(t, err)
	assertRanges(t, shards, [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}, {4, 5}})
	for _, shard := range shards {
		require.Equal(t, 2, shard.ParamCount)
		assertShardSql(t, shard)
	}
}

func TestSplitInsertSingleRowTooLarge(t *testing.T) {
	// ON CONFLICT adds one fixed parameter (the SET literal), repeated in every shard.
	stmt := table1.INSERT(table1Col1, table1ColBool).
		VALUES("a", true).
		ON_CONFLICT(table1ColBool).
		DO_UPDATE(SET(table1ColBool.SET(Bool(false))))

	// Fixed clauses need 1 parameter and the only row needs 2, but the limit is 2: the row does
	// not fit into the remaining budget of 1.
	shards, err := SplitInsertByParamBudget(stmt, 2, AcceptShardingSemantics())
	require.Error(t, err)
	require.Nil(t, shards)
	require.Contains(t, err.Error(), "row 1")
	require.Contains(t, err.Error(), "alone requires")

	// A middle row that is too large must not leave any partial plan behind.
	mixed := table1.INSERT(table1Col1, table1ColFloat, table1ColBool, table1ColInt).
		VALUES(1, 1.1, true, DEFAULT).        // 3 parameters
		VALUES(2, DEFAULT, DEFAULT, DEFAULT). // 1 parameter
		VALUES(3, 3.3, false, 4).             // 4 parameters, one more than the limit
		VALUES(5, 5.5, true, DEFAULT)         // 3 parameters

	shards, err = SplitInsertByParamBudget(mixed, 3, AcceptShardingSemantics())
	require.Error(t, err)
	require.Nil(t, shards)
	require.Contains(t, err.Error(), "row 3")

	// Fixed clauses alone already exceed the limit (SET and WHERE consume 2 fixed parameters).
	fixedTooLarge := table1.INSERT(table1Col1, table1ColBool).
		VALUES("a", true).
		ON_CONFLICT(table1Col1).
		DO_UPDATE(SET(table1ColBool.SET(Bool(false))).WHERE(table1Col1.GT(Int(0))))

	shards, err = SplitInsertByParamBudget(fixedTooLarge, 1, AcceptShardingSemantics())
	require.Error(t, err)
	require.Nil(t, shards)
	require.Contains(t, err.Error(), "fixed clauses alone require")
}

func TestSplitInsertDefaultMixed(t *testing.T) {
	stmt := table1.INSERT(table1Col1, table1ColFloat, table1ColBool).
		VALUES(1, DEFAULT, true).      // 2 parameters
		VALUES(DEFAULT, 2.2, DEFAULT). // 1 parameter
		VALUES(3, 3.3, false)          // 3 parameters

	shards, err := SplitInsertByParamBudget(stmt, 3, AcceptShardingSemantics())
	require.NoError(t, err)
	assertRanges(t, shards, [][2]int{{0, 2}, {2, 3}})
	require.Equal(t, []int{3, 3}, []int{shards[0].ParamCount, shards[1].ParamCount})

	firstQuery, firstArgs := assertShardSql(t, shards[0])
	require.Contains(t, firstQuery, "VALUES ($1, DEFAULT, $2),")
	require.Contains(t, firstQuery, "(DEFAULT, $3, DEFAULT)")
	require.Equal(t, []interface{}{1, true, 2.2}, firstArgs)

	secondQuery, secondArgs := assertShardSql(t, shards[1])
	require.Contains(t, secondQuery, "VALUES ($1, $2, $3)")
	require.Equal(t, []interface{}{3, 3.3, false}, secondArgs)
}

func TestSplitInsertFixedConflictParams(t *testing.T) {
	stmt := table1.INSERT(table1Col1, table1ColFloat).
		VALUES(1, 1.1).
		VALUES(2, 2.2).
		VALUES(3, 3.3).
		ON_CONFLICT(table1Col1).WHERE(table1ColBool.IS_NOT_FALSE()).
		DO_UPDATE(
			SET(table1ColFloat.SET(Float(9.9))).
				WHERE(table1Col1.GT(Int(0))),
		).
		RETURNING(table1Col1)

	// Fixed clauses: SET $n = 9.9 and WHERE ... > $n consume 2 parameters in every shard.
	shards, err := SplitInsertByParamBudget(stmt, 6, AcceptShardingSemantics())
	require.NoError(t, err)
	assertRanges(t, shards, [][2]int{{0, 2}, {2, 3}})
	require.Equal(t, []int{6, 4}, []int{shards[0].ParamCount, shards[1].ParamCount})

	for _, shard := range shards {
		query, args := assertShardSql(t, shard)
		require.Contains(t, query, "ON CONFLICT (col1) WHERE col_bool IS NOT FALSE")
		require.Contains(t, query, "DO UPDATE")
		require.Contains(t, query, "RETURNING")
		// The fixed parameters are repeated in each shard and come after the row parameters.
		require.Equal(t, 9.9, args[len(args)-2])
		require.Equal(t, int64(0), args[len(args)-1])
	}
}

func TestSplitInsertComplexExpressions(t *testing.T) {
	stmt := table1.INSERT(table1Col1, table1ColFloat).
		// COALESCE binds 2 parameters, column + addition binds 1: the row really consumes 3.
		VALUES(COALESCE(Int(1), Int(2)), table1ColFloat.ADD(Float(0.5))).
		// Nested CAST expression binds one parameter for the text literal.
		VALUES(Int(7), CAST(String("2.5")).AS_REAL()).
		// Fixed literals are inlined and consume no bound parameters.
		VALUES(jet.FixedLiteral(8), jet.FixedLiteral(8.8)).
		VALUES(9, 9.9)

	shards, err := SplitInsertByParamBudget(stmt, 3, AcceptShardingSemantics())
	require.NoError(t, err)
	// The zero-parameter fixed-literal row is packed together with the previous row instead of
	// forcing its own shard, proving the budget is driven by the serialized parameter count.
	assertRanges(t, shards, [][2]int{{0, 1}, {1, 3}, {3, 4}})
	require.Equal(t, []int{3, 2, 2},
		[]int{shards[0].ParamCount, shards[1].ParamCount, shards[2].ParamCount})

	firstQuery, _ := assertShardSql(t, shards[0])
	require.Contains(t, firstQuery, "COALESCE($1, $2)")
	require.Contains(t, firstQuery, "col_float + $3")

	secondQuery, secondArgs := assertShardSql(t, shards[1])
	require.Contains(t, secondQuery, "$2::text::real")
	require.Contains(t, secondQuery, "(8, 8.8)")
	require.Len(t, secondArgs, 2)

	thirdQuery, thirdArgs := assertShardSql(t, shards[2])
	require.Contains(t, thirdQuery, "VALUES ($1, $2)")
	require.Equal(t, []interface{}{9, 9.9}, thirdArgs)
}

func TestSplitInsertModels(t *testing.T) {
	type table1Model struct {
		Col1     int
		ColFloat float64
	}

	models := []table1Model{
		{1, 1.1},
		{2, 2.2},
		{3, 3.3},
	}

	stmt := table1.INSERT(table1Col1, table1ColFloat).MODELS(models)

	shards, err := SplitInsertByParamBudget(stmt, 4, AcceptShardingSemantics())
	require.NoError(t, err)
	assertRanges(t, shards, [][2]int{{0, 2}, {2, 3}})

	for _, shard := range shards {
		assertShardSql(t, shard)
	}
}

func TestSplitInsertReturningWithParameter(t *testing.T) {
	stmt := table1.INSERT(table1Col1, table1ColFloat).
		VALUES(1, 1.1).
		VALUES(2, 2.2).
		RETURNING(table1Col1, table1ColFloat.ADD(Float(100.0)))

	// RETURNING binds one fixed parameter in each shard.
	shards, err := SplitInsertByParamBudget(stmt, 3, AcceptShardingSemantics())
	require.NoError(t, err)
	assertRanges(t, shards, [][2]int{{0, 1}, {1, 2}})

	for _, shard := range shards {
		query, args := assertShardSql(t, shard)
		require.Equal(t, 3, shard.ParamCount)
		require.Contains(t, query, "RETURNING")
		require.Contains(t, query, "col_float + $3")
		require.Equal(t, 100.0, args[2])
	}
}

func TestSplitInsertRejections(t *testing.T) {
	t.Run("query insert", func(t *testing.T) {
		stmt := table1.INSERT(table1Col1).QUERY(table1.SELECT(table1Col1))

		shards, err := SplitInsertByParamBudget(stmt, 10,
			AcceptShardingSemantics(), AllowVolatileExpressions(), AllowDuplicateConflictKeys())
		require.Error(t, err)
		require.Nil(t, shards)
		require.Contains(t, err.Error(), "QUERY")
	})

	t.Run("no values rows", func(t *testing.T) {
		stmt := table1.INSERT(table1Col1)

		shards, err := SplitInsertByParamBudget(stmt, 10, AcceptShardingSemantics())
		require.Error(t, err)
		require.Nil(t, shards)
	})

	t.Run("non positive limit", func(t *testing.T) {
		stmt := table1.INSERT(table1Col1).VALUES(1)

		shards, err := SplitInsertByParamBudget(stmt, -1, AcceptShardingSemantics())
		require.Error(t, err)
		require.Nil(t, shards)
	})

	t.Run("sharding semantics not accepted", func(t *testing.T) {
		stmt := table1.INSERT(table1Col1).VALUES(1).VALUES(2)

		shards, err := SplitInsertByParamBudget(stmt, 10)
		require.Error(t, err)
		require.Nil(t, shards)
		require.Contains(t, err.Error(), "AcceptShardingSemantics")
	})
}

func TestSplitInsertVolatileAndOpaqueOptIn(t *testing.T) {
	t.Run("raw expression in row", func(t *testing.T) {
		stmt := table1.INSERT(table1Col1).
			VALUES(RawInt("1 + $offset", map[string]interface{}{"offset": 10})).
			VALUES(2)

		shards, err := SplitInsertByParamBudget(stmt, 10, AcceptShardingSemantics())
		require.Error(t, err)
		require.Nil(t, shards)
		require.Contains(t, err.Error(), "input row 1")

		shards, err = SplitInsertByParamBudget(stmt, 10, AcceptShardingSemantics(), AllowVolatileExpressions())
		require.NoError(t, err)
		assertRanges(t, shards, [][2]int{{0, 2}})
		assertShardSql(t, shards[0])
	})

	t.Run("volatile function", func(t *testing.T) {
		stmt := table1.INSERT(table1Col1, table1ColFloat).
			VALUES(Func("RANDOM"), 1.1).
			VALUES(2, 2.2)

		shards, err := SplitInsertByParamBudget(stmt, 10, AcceptShardingSemantics())
		require.Error(t, err)
		require.Nil(t, shards)
		require.Contains(t, err.Error(), "RANDOM")

		shards, err = SplitInsertByParamBudget(stmt, 10, AcceptShardingSemantics(), AllowVolatileExpressions())
		require.NoError(t, err)
		assertRanges(t, shards, [][2]int{{0, 2}})
	})

	t.Run("embedded statement in row", func(t *testing.T) {
		stmt := table1.INSERT(table1Col1).
			VALUES(table1.SELECT(table1Col1)).
			VALUES(2)

		shards, err := SplitInsertByParamBudget(stmt, 10, AcceptShardingSemantics())
		require.Error(t, err)
		require.Nil(t, shards)
		require.Contains(t, err.Error(), "sub-query")

		shards, err = SplitInsertByParamBudget(stmt, 10, AcceptShardingSemantics(), AllowVolatileExpressions())
		require.NoError(t, err)
		assertRanges(t, shards, [][2]int{{0, 2}})
	})

	t.Run("raw expression in on conflict", func(t *testing.T) {
		stmt := table1.INSERT(table1Col1, table1ColBool).
			VALUES(1, true).
			ON_CONFLICT(table1Col1).WHERE(RawBool("true")).
			DO_NOTHING()

		shards, err := SplitInsertByParamBudget(stmt, 10, AcceptShardingSemantics())
		require.Error(t, err)
		require.Nil(t, shards)
		require.Contains(t, err.Error(), "ON CONFLICT clause")

		shards, err = SplitInsertByParamBudget(stmt, 10, AcceptShardingSemantics(), AllowVolatileExpressions())
		require.NoError(t, err)
		assertShardSql(t, shards[0])
	})

	t.Run("raw expression in returning", func(t *testing.T) {
		stmt := table1.INSERT(table1Col1).
			VALUES(1).
			RETURNING(RawInt("1"))

		shards, err := SplitInsertByParamBudget(stmt, 10, AcceptShardingSemantics())
		require.Error(t, err)
		require.Nil(t, shards)
		require.Contains(t, err.Error(), "RETURNING clause")

		shards, err = SplitInsertByParamBudget(stmt, 10, AcceptShardingSemantics(), AllowVolatileExpressions())
		require.NoError(t, err)
		assertShardSql(t, shards[0])
	})
}

func TestSplitInsertDuplicateConflictKeys(t *testing.T) {
	build := func() InsertStatement {
		return table1.INSERT(table1Col1, table1ColBool).
			VALUES(1, true).
			VALUES(1, false).
			VALUES(2, true).
			ON_CONFLICT(table1Col1).
			DO_UPDATE(SET(table1ColBool.SET(Bool(true))))
	}

	shards, err := SplitInsertByParamBudget(build(), 10, AcceptShardingSemantics())
	require.Error(t, err)
	require.Nil(t, shards)
	require.Contains(t, err.Error(), "rows 1 and 2")
	require.Contains(t, err.Error(), "AllowDuplicateConflictKeys")

	shards, err = SplitInsertByParamBudget(build(), 10,
		AcceptShardingSemantics(), AllowDuplicateConflictKeys())
	require.NoError(t, err)
	assertRanges(t, shards, [][2]int{{0, 3}})
	assertShardSql(t, shards[0])

	// When the key cannot be proven literal (DEFAULT), detection is skipped instead of failing.
	undecidable := table1.INSERT(table1Col1, table1ColBool).
		VALUES(DEFAULT, true).
		VALUES(DEFAULT, false).
		ON_CONFLICT(table1Col1).DO_NOTHING()

	shards, err = SplitInsertByParamBudget(undecidable, 10, AcceptShardingSemantics())
	require.NoError(t, err)
	assertRanges(t, shards, [][2]int{{0, 2}})
}

func TestSplitInsertPreservesOriginalStatement(t *testing.T) {
	stmt := table1.INSERT(table1Col1, table1ColFloat).
		VALUES(1, 1.1).
		VALUES(2, 2.2).
		VALUES(3, 3.3).
		ON_CONFLICT(table1Col1).DO_NOTHING().
		RETURNING(table1Col1)

	originalSql, originalArgs := stmt.Sql()
	originalDebugSql := stmt.DebugSql()

	impl := stmt.(*insertStatementImpl)
	originalRowCount := len(impl.ValuesQuery.Rows)

	shards, err := SplitInsertByParamBudget(stmt, 4, AcceptShardingSemantics())
	require.NoError(t, err)
	require.NotEmpty(t, shards)

	// The source statement, its rows, models and expressions are untouched.
	sqlAfter, argsAfter := stmt.Sql()
	require.Equal(t, originalSql, sqlAfter)
	require.Equal(t, originalArgs, argsAfter)
	require.Equal(t, originalDebugSql, stmt.DebugSql())
	require.Equal(t, originalRowCount, len(impl.ValuesQuery.Rows))

	// Repeated compilation of the source statement and of every shard is deterministic.
	sqlAgain, argsAgain := stmt.Sql()
	require.Equal(t, originalSql, sqlAgain)
	require.Equal(t, originalArgs, argsAgain)

	for _, shard := range shards {
		queryOne, argsOne := shard.Statement.Sql()
		queryTwo, argsTwo := shard.Statement.Sql()
		require.Equal(t, queryOne, queryTwo)
		require.Equal(t, argsOne, argsTwo)
	}

	// Splitting twice yields identical, independent plans.
	shardsAgain, err := SplitInsertByParamBudget(stmt, 4, AcceptShardingSemantics())
	require.NoError(t, err)
	require.Len(t, shardsAgain, len(shards))

	for index := range shards {
		queryOne, argsOne := shards[index].Statement.Sql()
		queryTwo, argsTwo := shardsAgain[index].Statement.Sql()
		require.Equal(t, queryOne, queryTwo)
		require.Equal(t, argsOne, argsTwo)
		require.Equal(t, shards[index].RowStart, shardsAgain[index].RowStart)
		require.Equal(t, shards[index].RowEnd, shardsAgain[index].RowEnd)
		require.Equal(t, shards[index].ParamCount, shardsAgain[index].ParamCount)
	}
}
