package postgres

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-jet/jet/v2/internal/jet"
	"github.com/stretchr/testify/require"
)

// TestChecked_PostgresPrepareCrossCheck validates the static checker against a real PostgreSQL
// server using PREPARE, which parses and analyzes a statement without executing it. The test skips
// itself when no PostgreSQL instance is reachable, so the package suite stays green in environments
// without a database. Point it at any instance with JET_TEST_PG_DSN, otherwise the repository
// default (localhost:50901, see tests/docker-compose.yaml) is used.
func TestChecked_PostgresPrepareCrossCheck(t *testing.T) {
	dsn := os.Getenv("JET_TEST_PG_DSN")
	if dsn == "" {
		dsn = "host=localhost port=50901 user=jet password=jet dbname=jetdb sslmode=disable"
	}

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()

	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := db.PingContext(pingCtx); err != nil {
		t.Skipf("PostgreSQL not reachable (%v), skipping PREPARE cross-check", err)
	}

	// Temporary tables live for the session and are dropped automatically.
	_, err = db.Exec(`
CREATE TEMP TABLE t_a (id integer, name text);
CREATE TEMP TABLE t_b (id integer, a_id integer, label text);
CREATE TEMP TABLE t_c (id integer, b_id integer);
CREATE TEMP TABLE t_base (base_id integer);
CREATE TEMP TABLE t_side (side_id integer);
CREATE TEMP TABLE t_dst (id integer);
CREATE TEMP TABLE "Weird.Table" ("Weird.Col" integer);
`)
	require.NoError(t, err)

	aID := IntegerColumn("id")
	aName := StringColumn("name")
	tableA := NewTable("", "t_a", "a", aID, aName)

	bID := IntegerColumn("id")
	bAID := IntegerColumn("a_id")
	bLabel := StringColumn("label")
	tableB := NewTable("", "t_b", "b", bID, bAID, bLabel)

	cID := IntegerColumn("id")
	cBID := IntegerColumn("b_id")
	tableC := NewTable("", "t_c", "c", cID, cBID)

	baseID := IntegerColumn("base_id")
	baseTable := NewTable("", "t_base", "b", baseID)

	sideID := IntegerColumn("side_id")
	sideTable := NewTable("", "t_side", "s", sideID)

	dstID := IntegerColumn("id")
	dstTable := NewTable("", "t_dst", "", dstID)

	weirdCol := IntegerColumn("Weird.Col")
	weirdTable := NewTable("", "Weird.Table", "", weirdCol)

	legal := []struct {
		name string
		stmt jet.SerializerStatement
	}{
		{
			name: "select where",
			stmt: SELECT(aID, aName).FROM(tableA).WHERE(aID.EQ(Int(1))),
		},
		{
			name: "join on two sides",
			stmt: SELECT(aID, bID).
				FROM(tableA.INNER_JOIN(tableB, aID.EQ(bAID))).
				WHERE(bLabel.EQ(String("x"))),
		},
		{
			name: "chained joins",
			stmt: SELECT(aID).
				FROM(tableA.
					INNER_JOIN(tableB, aID.EQ(bAID)).
					INNER_JOIN(tableC, bID.EQ(cBID))),
		},
		{
			name: "correlated scalar subquery",
			stmt: SELECT(aID).FROM(tableA).
				WHERE(EXISTS(SELECT(bID).FROM(tableB).WHERE(bAID.EQ(aID)))),
		},
		{
			name: "non lateral derived table",
			stmt: SELECT(aID).FROM(tableA, SELECT(bID).FROM(tableB).AsTable("d")),
		},
		{
			name: "lateral left correlation",
			stmt: SELECT(baseID).FROM(baseTable,
				LATERAL(SELECT(sideID).FROM(sideTable).WHERE(sideID.EQ(baseID))).AS("lat")),
		},
		{
			name: "cte chain",
			stmt: func() jet.SerializerStatement {
				c1 := CTE("c1")
				c2 := CTE("c2")
				return WITH(
					c1.AS(SELECT(aID).FROM(tableA)),
					c2.AS(SELECT(bID).FROM(tableB).WHERE(bAID.IN(SELECT(c1.AllColumns()).FROM(c1)))),
				)(
					SELECT(c2.AllColumns()).FROM(c2),
				)
			}(),
		},
		{
			name: "recursive cte",
			stmt: func() jet.SerializerStatement {
				n := IntegerColumn("n")
				rec := CTE("rec", n)
				return WITH_RECURSIVE(
					rec.AS(
						SELECT(Int32(1)).UNION_ALL(
							SELECT(n.ADD(Int(1))).FROM(rec).WHERE(n.LT(Int(5))),
						),
					),
				)(
					SELECT(rec.AllColumns()).FROM(rec),
				)
			}(),
		},
		{
			name: "insert query",
			stmt: dstTable.INSERT(dstID).QUERY(SELECT(aID).FROM(tableA).WHERE(aID.GT(Int(0)))),
		},
		{
			name: "dotted quoted identifiers",
			stmt: SELECT(weirdCol).FROM(weirdTable).WHERE(weirdCol.EQ(Int(7))),
		},
	}

	for _, tc := range legal {
		t.Run("legal/"+tc.name, func(t *testing.T) {
			result, err := Checked(tc.stmt)
			require.NoError(t, err, "static check should pass")
			require.Equal(t, jet.StatusChecked, result.Report.Status())

			// Repeated checked serialization must be byte identical.
			repeated, repeatErr := Checked(tc.stmt)
			require.NoError(t, repeatErr)
			require.Equal(t, result.SQL, repeated.SQL)
			require.Equal(t, result.Args, repeated.Args)

			prepareName := "chk_legal_" + strings.ReplaceAll(tc.name, " ", "_")
			prepareSQL := "PREPARE " + prepareName + " AS " + strings.TrimSuffix(result.SQL, ";\n")
			_, execErr := db.Exec(prepareSQL)
			require.NoError(t, execErr, "PostgreSQL PREPARE should accept: %s", result.SQL)
			_, deallocErr := db.Exec("DEALLOCATE " + prepareName)
			require.NoError(t, deallocErr)
		})
	}

	ghostID := IntegerColumn("id")
	newCheckedTable("", "t_ghost", "g", ghostID)

	missingCol := StringColumn("label")
	jet.SetTableName(missingCol, "a")

	dupFirstID := IntegerColumn("id")
	dupFirst := NewTable("", "t_a", "dup", dupFirstID)
	dupSecondID := IntegerColumn("id")
	dupSecond := NewTable("", "t_b", "dup", dupSecondID)

	illegal := []struct {
		name string
		stmt jet.SerializerStatement
		kind jet.ReferenceErrorKind
	}{
		{
			name: "unknown relation",
			stmt: SELECT(aID).FROM(tableA).WHERE(ghostID.EQ(Int(1))),
			kind: jet.ErrUnknownRelation,
		},
		{
			name: "unknown column",
			stmt: SELECT(aID).FROM(tableA).WHERE(missingCol.IS_NULL()),
			kind: jet.ErrUnknownColumn,
		},
		{
			name: "duplicate alias",
			stmt: SELECT(dupFirstID).FROM(dupFirst, dupSecond),
			kind: jet.ErrDuplicateRelation,
		},
		{
			name: "non lateral sibling correlation",
			stmt: SELECT(baseID).FROM(
				baseTable,
				SELECT(sideID).FROM(sideTable).WHERE(sideID.EQ(baseID)).AsTable("d"),
			),
			kind: jet.ErrIllegalCorrelation,
		},
		{
			name: "cte forward reference",
			stmt: func() jet.SerializerStatement {
				early := CTE("early")
				later := CTE("later", IntegerColumn("id"))
				return WITH(
					early.AS(SELECT(later.AllColumns()).FROM(later)),
					later.AS(SELECT(aID).FROM(tableA)),
				)(
					SELECT(aID).FROM(tableA),
				)
			}(),
			kind: jet.ErrUnknownRelation,
		},
	}

	for _, tc := range illegal {
		t.Run("illegal/"+tc.name, func(t *testing.T) {
			result, err := Checked(tc.stmt)
			require.Error(t, err, "static check should fail")
			require.Nil(t, result)

			var checkedErr *jet.CheckedError
			require.ErrorAs(t, err, &checkedErr)
			requireErrorKind(t, checkedErr.Report, tc.kind)

			// PostgreSQL PREPARE must reject the same statement.
			sqlText, _ := tc.stmt.Sql()
			prepareName := "chk_illegal_" + strings.ReplaceAll(tc.name, " ", "_")
			_, prepareErr := db.Exec("PREPARE " + prepareName + " AS " + strings.TrimSuffix(sqlText, ";\n"))
			require.Error(t, prepareErr, "PostgreSQL PREPARE should reject: %s", sqlText)
		})
	}
}
