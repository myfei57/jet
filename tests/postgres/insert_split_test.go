package postgres

import (
	"testing"

	"github.com/go-jet/jet/v2/internal/testutils"
	. "github.com/go-jet/jet/v2/postgres"
	"github.com/go-jet/jet/v2/qrm"
	"github.com/stretchr/testify/require"
)

// TestInsertSplitByParamBudget executes every generated shard against a temporary table and
// verifies that exact per-shard parameter counts, row ranges, DEFAULT handling and the per-shard
// RETURNING sequence all hold at runtime.
func TestInsertSplitByParamBudget(t *testing.T) {
	idCol := IntegerColumn("id")
	nameCol := StringColumn("name")
	ratioCol := FloatColumn("ratio")
	activeCol := BoolColumn("active")
	tempTable := NewTable("", "jet_split_insert_temp", "", idCol, nameCol, ratioCol, activeCol)

	type splitRow struct {
		ID     int32
		Name   string
		Ratio  *float64
		Active bool
	}

	testutils.ExecuteInTxAndRollback(t, db, func(tx qrm.DB) {
		_, err := tx.Exec(`CREATE TEMP TABLE jet_split_insert_temp (
			id integer NOT NULL PRIMARY KEY,
			name text NOT NULL,
			ratio double precision,
			active boolean NOT NULL DEFAULT true
		)`)
		require.NoError(t, err)

		stmt := tempTable.INSERT(idCol, nameCol, ratioCol, activeCol).
			VALUES(1, "one", 1.5, DEFAULT).  // 3 parameters
			VALUES(2, "two", DEFAULT, true). // 3 parameters
			VALUES(3, "three", 3.5, false).  // 4 parameters
			VALUES(4, "four", 4.5, true).    // 4 parameters
			RETURNING(idCol, nameCol, ratioCol, activeCol)

		shards, err := SplitInsertByParamBudget(stmt, 7, AcceptShardingSemantics())
		require.NoError(t, err)
		require.Len(t, shards, 3)
		require.Equal(t, [][2]int{{0, 2}, {2, 3}, {3, 4}},
			[][2]int{{shards[0].RowStart, shards[0].RowEnd}, {shards[1].RowStart, shards[1].RowEnd}, {shards[2].RowStart, shards[2].RowEnd}})
		require.Equal(t, []int{6, 4, 4},
			[]int{shards[0].ParamCount, shards[1].ParamCount, shards[2].ParamCount})

		var returnedRows int

		for _, shard := range shards {
			query, args := shard.Statement.Sql()
			require.Len(t, args, shard.ParamCount)
			require.LessOrEqual(t, shard.ParamCount, 7)
			require.LessOrEqual(t, len(args), 7)

			// RETURNING only guarantees the sequence inside the current shard.
			var returned []splitRow
			require.NoError(t, shard.Statement.Query(tx, &returned), query)
			require.Len(t, returned, shard.RowEnd-shard.RowStart)
			returnedRows += len(returned)
		}

		require.Equal(t, 4, returnedRows)

		var stored []splitRow
		require.NoError(t, tempTable.SELECT(idCol, nameCol, ratioCol, activeCol).
			ORDER_BY(idCol).Query(tx, &stored))
		require.Len(t, stored, 4)

		require.Equal(t, int32(1), stored[0].ID)
		require.Equal(t, "one", stored[0].Name)
		require.NotNil(t, stored[0].Ratio)
		require.Equal(t, 1.5, *stored[0].Ratio)
		require.True(t, stored[0].Active) // column DEFAULT applied

		require.Equal(t, int32(2), stored[1].ID)
		require.Nil(t, stored[1].Ratio) // row DEFAULT rendered as SQL DEFAULT
		require.True(t, stored[1].Active)

		require.False(t, stored[2].Active)
		require.Equal(t, int32(4), stored[3].ID)
		require.True(t, stored[3].Active)
	})
}

// TestInsertSplitByParamBudgetModels checks the MODELS entry point end to end, including the
// repeated fixed ON CONFLICT parameters charged to every shard.
func TestInsertSplitByParamBudgetModels(t *testing.T) {
	idCol := IntegerColumn("id")
	nameCol := StringColumn("name")
	activeCol := BoolColumn("active")
	tempTable := NewTable("", "jet_split_models_temp", "", idCol, nameCol, activeCol)

	type splitModel struct {
		ID     int32
		Name   string
		Active bool
	}

	testutils.ExecuteInTxAndRollback(t, db, func(tx qrm.DB) {
		_, err := tx.Exec(`CREATE TEMP TABLE jet_split_models_temp (
			id integer NOT NULL PRIMARY KEY,
			name text NOT NULL,
			active boolean NOT NULL
		)`)
		require.NoError(t, err)

		models := []splitModel{
			{ID: 10, Name: "ten", Active: true},
			{ID: 11, Name: "eleven", Active: false},
			{ID: 12, Name: "twelve", Active: true},
			{ID: 13, Name: "thirteen", Active: false},
		}

		// The DO UPDATE SET literal adds one fixed parameter to every shard.
		stmt := tempTable.INSERT(idCol, nameCol, activeCol).
			MODELS(models).
			ON_CONFLICT(idCol).DO_UPDATE(SET(activeCol.SET(Bool(true))))

		shards, err := SplitInsertByParamBudget(stmt, 7, AcceptShardingSemantics())
		require.NoError(t, err)
		require.Len(t, shards, 2)
		require.Equal(t, [][2]int{{0, 2}, {2, 4}},
			[][2]int{{shards[0].RowStart, shards[0].RowEnd}, {shards[1].RowStart, shards[1].RowEnd}})
		// Each shard: 2 rows * 3 parameters + 1 fixed conflict parameter.
		require.Equal(t, 7, shards[0].ParamCount)
		require.Equal(t, 7, shards[1].ParamCount)

		for _, shard := range shards {
			_, args := shard.Statement.Sql()
			require.Len(t, args, shard.ParamCount)
			_, execErr := shard.Statement.Exec(tx)
			require.NoError(t, execErr)
		}

		var count int
		require.NoError(t, tempTable.SELECT(COUNT(STAR)).Query(tx, &count))
		require.Equal(t, 4, count)

		var ids []int32
		require.NoError(t, tempTable.SELECT(idCol).ORDER_BY(idCol).Query(tx, &ids))
		require.Equal(t, []int32{10, 11, 12, 13}, ids)
	})
}

// TestInsertSplitByParamBudgetOnConflict runs shards whose input rows repeat the same conflict
// key, which is only allowed after the explicit duplicate-key opt-in.
func TestInsertSplitByParamBudgetOnConflict(t *testing.T) {
	idCol := IntegerColumn("id")
	nameCol := StringColumn("name")
	tempTable := NewTable("", "jet_split_upsert_temp", "", idCol, nameCol)

	type upsertRow struct {
		ID   int32
		Name string
	}

	testutils.ExecuteInTxAndRollback(t, db, func(tx qrm.DB) {
		_, err := tx.Exec(`CREATE TEMP TABLE jet_split_upsert_temp (
			id integer NOT NULL PRIMARY KEY,
			name text NOT NULL
		)`)
		require.NoError(t, err)

		t.Run("do nothing", func(t *testing.T) {
			stmt := tempTable.INSERT(idCol, nameCol).
				VALUES(1, "a").VALUES(1, "b").VALUES(2, "c").VALUES(2, "d").
				ON_CONFLICT(idCol).DO_NOTHING()

			shards, splitErr := SplitInsertByParamBudget(stmt, 3,
				AcceptShardingSemantics(), AllowDuplicateConflictKeys())
			require.NoError(t, splitErr)
			require.Len(t, shards, 4)

			for _, shard := range shards {
				_, args := shard.Statement.Sql()
				require.Len(t, args, shard.ParamCount)
				require.NoError(t, shard.Statement.Exec(tx))
			}

			var stored []upsertRow
			require.NoError(t, tempTable.SELECT(idCol, nameCol).ORDER_BY(idCol).Query(tx, &stored))
			require.Equal(t, []upsertRow{{ID: 1, Name: "a"}, {ID: 2, Name: "c"}}, stored)
		})

		t.Run("do update", func(t *testing.T) {
			stmt := tempTable.INSERT(idCol, nameCol).
				VALUES(3, "e").VALUES(3, "f").VALUES(4, "g").VALUES(4, "h").
				ON_CONFLICT(idCol).DO_UPDATE(SET(nameCol.SET(String("updated"))))

			// Fixed SET parameter leaves a budget of 2 row parameters per shard.
			shards, splitErr := SplitInsertByParamBudget(stmt, 3,
				AcceptShardingSemantics(), AllowDuplicateConflictKeys())
			require.NoError(t, splitErr)
			require.Len(t, shards, 4)
			for _, shard := range shards {
				require.Equal(t, 3, shard.ParamCount)
				require.NoError(t, shard.Statement.Exec(tx))
			}

			var stored []upsertRow
			require.NoError(t, tempTable.SELECT(idCol, nameCol).
				WHERE(idCol.GT(Int(2))).ORDER_BY(idCol).Query(tx, &stored))
			require.Equal(t, []upsertRow{{ID: 3, Name: "updated"}, {ID: 4, Name: "updated"}}, stored)
		})
	})
}
