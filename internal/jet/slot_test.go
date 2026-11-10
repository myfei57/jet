package jet

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// questionDialect is a dialect using positional '?' placeholders without placeholder reuse,
// mimicking MySQL and SQLite.
var questionDialect = NewDialect(DialectParams{
	AliasQuoteChar:      '"',
	IdentifierQuoteChar: '`',
	ArgumentPlaceholder: func(int) string { return "?" },
	// ReuseArgumentPlaceholders defaults to false.
	ArgumentToString: func(value any) (string, bool) { return "", false },
})

func newTestSelectStatement(dialect Dialect, clauses ...Clause) *statementImpl {
	stmt := &statementImpl{Clauses: clauses}
	stmt.statementInterfaceImpl = statementInterfaceImpl{
		root:          stmt,
		dialect:       dialect,
		statementType: SelectStatementType,
	}
	return stmt
}

func slotSelectStatement(dialect Dialect, condition BoolExpression) SerializerStatement {
	return newTestSelectStatement(dialect,
		&ClauseSelect{ProjectionList: []Projection{table1Col1}},
		&ClauseFrom{Tables: []Serializer{table1}},
		&ClauseWhere{Condition: condition},
	)
}

// compileExpression serializes a single expression through a slot-aware builder and returns
// the rendered text together with the frozen compiler.
func compileExpression(t *testing.T, dialect Dialect, expression Expression) (string, *slotCompiler) {
	t.Helper()

	compiler := newSlotCompiler(dialect.ReuseArgumentPlaceholders())
	builder := &SQLBuilder{Dialect: dialect, slotCompiler: compiler}
	expression.serialize(SelectStatementType, builder)

	return builder.Buff.String(), compiler
}

func TestSlot_RepeatedSlotReusesPlaceholderInPostgresDialect(t *testing.T) {
	slot := NewIntSlot("repeated")

	expression := NewBinaryOperatorExpression(slot, slot, "+")

	query, compiler := compileExpression(t, defaultDialect, expression)
	require.Equal(t, "($1 + $1)", query)
	require.Len(t, compiler.plan, 1)
	require.Equal(t, "repeated", compiler.slotsInOrder[0].name)
}

func TestSlot_RepeatedSlotCopiesArgumentsInQuestionDialect(t *testing.T) {
	slot := NewIntSlot("repeated")

	expression := NewBinaryOperatorExpression(slot, slot, "+")

	query, compiler := compileExpression(t, questionDialect, expression)
	require.Equal(t, "(? + ?)", query)
	require.Len(t, compiler.plan, 2)
	require.Same(t, compiler.plan[0].slot, compiler.plan[1].slot)
}

func TestSlot_MixedLiteralsAndSlotsKeepOrderAndCount(t *testing.T) {
	slot := NewIntSlot("slot_value")
	literal := Int(9)

	// ($1 = ($2 + $2)) in postgres, (? = (? + ?)) in question dialects.
	expression := NewBinaryOperatorExpression(
		literal,
		NewBinaryOperatorExpression(slot, slot, "+"),
		"=",
	)

	pgQuery, pgCompiler := compileExpression(t, defaultDialect, expression)
	require.Equal(t, "($1 = ($2 + $2))", pgQuery)
	require.Len(t, pgCompiler.plan, 2)
	require.Nil(t, pgCompiler.plan[0].slot)
	require.Equal(t, int64(9), pgCompiler.plan[0].value)
	require.NotNil(t, pgCompiler.plan[1].slot)

	qmQuery, qmCompiler := compileExpression(t, questionDialect, expression)
	require.Equal(t, "(? = (? + ?))", qmQuery)
	require.Len(t, qmCompiler.plan, 3)
	require.Nil(t, qmCompiler.plan[0].slot)
	require.Same(t, qmCompiler.plan[1].slot, qmCompiler.plan[2].slot)

	compiled, err := slotSelectStatement(defaultDialect, Eq(literal, NewBinaryOperatorExpression(slot, slot, "+"))).Compile()
	require.NoError(t, err)

	bound, err := compiled.Bind(SlotValue(slot, int64(3)))
	require.NoError(t, err)

	query, args := bound.Sql()
	require.Contains(t, query, "$1 = ($2 + $2)")
	require.Equal(t, []any{int64(9), int64(3)}, args)
}

func TestSlot_QuestionDialectRepeatsBoundArgumentPerOccurrence(t *testing.T) {
	slot := NewIntSlot("slot_value")

	compiled, err := slotSelectStatement(questionDialect,
		Eq(Int(9), NewBinaryOperatorExpression(slot, slot, "+"))).Compile()
	require.NoError(t, err)

	bound, err := compiled.Bind(SlotValue(slot, int64(3)))
	require.NoError(t, err)

	query, args := bound.Sql()
	require.Contains(t, query, "? = (? + ?)")
	require.Equal(t, []any{int64(9), int64(3), int64(3)}, args)
}

func TestSlot_SameNameDifferentSlotsAreNotMerged(t *testing.T) {
	first := NewIntSlot("same")
	second := NewStringSlot("same")
	require.NotSame(t, first, second)

	expression := Eq(first, second)

	query, compiler := compileExpression(t, defaultDialect, expression)
	require.Equal(t, "($1 = $2)", query)
	require.Len(t, compiler.plan, 2)
	require.NotSame(t, compiler.plan[0].slot, compiler.plan[1].slot)

	compiled, err := slotSelectStatement(defaultDialect, expression).Compile()
	require.NoError(t, err)

	require.Len(t, compiled.Slots(), 2)
	require.Equal(t, "same", compiled.Slots()[0].SlotName())
	require.Equal(t, "same", compiled.Slots()[1].SlotName())

	bound, err := compiled.Bind(
		SlotValue(first, int64(1)),
		SlotValue(second, "text"),
	)
	require.NoError(t, err)

	_, args := bound.Sql()
	require.Equal(t, []any{int64(1), "text"}, args)

	// Binding the second slot with an incompatible type fails.
	_, err = compiled.Bind(
		SlotValue(first, int64(1)),
		SlotValue(second, 7),
	)
	require.ErrorContains(t, err, `slot "same" expects string value, got int`)
}

func TestSlot_CompileFreezesSqlAcrossMultipleBinds(t *testing.T) {
	slot := NewIntSlot("id")

	compiled, err := slotSelectStatement(defaultDialect, table1Col1.EQ(slot)).Compile()
	require.NoError(t, err)

	first, err := compiled.Bind(SlotValue(slot, int64(1)))
	require.NoError(t, err)

	second, err := compiled.Bind(SlotValue(slot, int64(2)))
	require.NoError(t, err)

	firstQuery, firstArgs := first.Sql()
	secondQuery, secondArgs := second.Sql()

	require.Equal(t, firstQuery, secondQuery)
	require.Equal(t, []any{int64(1)}, firstArgs)
	require.Equal(t, []any{int64(2)}, secondArgs)
}

func TestSlot_DeepWithBindsSharedSlotOnce(t *testing.T) {
	shared := NewIntSlot("shared_id")

	cteSelect := newTestSelectStatement(defaultDialect,
		&ClauseSelect{ProjectionList: []Projection{table1Col1}},
		&ClauseFrom{Tables: []Serializer{table1}},
		&ClauseWhere{Condition: table1Col1.GT(shared)},
	)

	cte := CTE("filtered")
	cte.Statement = cteSelect

	primary := newTestSelectStatement(defaultDialect,
		&ClauseSelect{ProjectionList: []Projection{table1Col1}},
		&ClauseFrom{Tables: []Serializer{table1}},
		&ClauseWhere{Condition: table1Col1.EQ(shared)},
	)

	withStatement := WITH(defaultDialect, false, &cte)(primary)

	compiled, err := withStatement.Compile()
	require.NoError(t, err)

	query := compiled.Sql()
	require.Equal(t, 2, strings.Count(query, "$1"))
	require.NotContains(t, query, "$2")
	require.Len(t, compiled.Slots(), 1)

	bound, err := compiled.Bind(SlotValue(shared, int64(42)))
	require.NoError(t, err)

	_, args := bound.Sql()
	require.Equal(t, []any{int64(42)}, args)

	// Question dialects copy the argument for every occurrence.
	cteSelectQuestion := newTestSelectStatement(questionDialect,
		&ClauseSelect{ProjectionList: []Projection{table1Col1}},
		&ClauseFrom{Tables: []Serializer{table1}},
		&ClauseWhere{Condition: table1Col1.GT(shared)},
	)

	cteQuestion := CTE("filtered")
	cteQuestion.Statement = cteSelectQuestion

	primaryQuestion := newTestSelectStatement(questionDialect,
		&ClauseSelect{ProjectionList: []Projection{table1Col1}},
		&ClauseFrom{Tables: []Serializer{table1}},
		&ClauseWhere{Condition: table1Col1.EQ(shared)},
	)

	compiledQuestion, err := WITH(questionDialect, false, &cteQuestion)(primaryQuestion).Compile()
	require.NoError(t, err)

	boundQuestion, err := compiledQuestion.Bind(SlotValue(shared, int64(42)))
	require.NoError(t, err)

	_, questionArgs := boundQuestion.Sql()
	require.Equal(t, []any{int64(42), int64(42)}, questionArgs)
}

func TestSlot_MissingExtraAndDuplicateBindings(t *testing.T) {
	first := NewIntSlot("first")
	second := NewStringSlot("second")

	compiled, err := slotSelectStatement(defaultDialect, Eq(first, second)).Compile()
	require.NoError(t, err)

	_, err = compiled.Bind(SlotValue(first, int64(1)))
	require.EqualError(t, err, `jet: missing value for slot "second"`)

	other := NewIntSlot("other")
	_, err = compiled.Bind(
		SlotValue(first, int64(1)),
		SlotValue(second, "text"),
		SlotValue(other, int64(3)),
	)
	require.EqualError(t, err, `jet: slot "other" does not appear in the compiled statement`)

	_, err = compiled.Bind(
		SlotValue(first, int64(1)),
		SlotValue(first, int64(2)),
		SlotValue(second, "text"),
	)
	require.EqualError(t, err, `jet: slot "first" is bound more than once`)

	_, err = compiled.Bind()
	require.ErrorContains(t, err, `missing value for slot "first"`)
}

func TestSlot_TypeCompatibility(t *testing.T) {
	intSlot := NewIntSlot("int_slot")
	floatSlot := NewFloatSlot("float_slot")
	boolSlot := NewBoolSlot("bool_slot")
	stringSlot := NewStringSlot("string_slot")
	timeSlot := NewTimestampSlot("time_slot")
	blobSlot := NewBlobSlot("blob_slot")

	compiled, err := slotSelectStatement(defaultDialect,
		BoolExp(NewBinaryOperatorExpression(
			NewBinaryOperatorExpression(
				NewBinaryOperatorExpression(intSlot, floatSlot, "+"),
				boolSlot, "AND",
			),
			NewBinaryOperatorExpression(stringSlot, NewBinaryOperatorExpression(timeSlot, blobSlot, ","), "AND"),
			"AND",
		)),
	).Compile()
	require.NoError(t, err)

	now := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

	bound, err := compiled.Bind(
		SlotValue(intSlot, int32(1)),
		SlotValue(floatSlot, 1.5),
		SlotValue(boolSlot, true),
		SlotValue(stringSlot, "text"),
		SlotValue(timeSlot, now),
		SlotValue(blobSlot, []byte("bytes")),
	)
	require.NoError(t, err)

	_, boundArgs := bound.Sql()
	require.Len(t, boundArgs, 6)

	cases := []struct {
		name    string
		slot    Slot
		value   any
		message string
	}{
		{"string into int slot", intSlot, "not-an-int", `slot "int_slot" expects integer value, got string`},
		{"int into float slot", floatSlot, int64(1), `slot "float_slot" expects float value, got int64`},
		{"string into bool slot", boolSlot, "true", `slot "bool_slot" expects boolean value, got string`},
		{"int into string slot", stringSlot, int64(1), `slot "string_slot" expects string value, got int64`},
		{"string into time slot", timeSlot, "2020-01-02", `slot "time_slot" expects time value, got string`},
		{"int into blob slot", blobSlot, int64(1), `slot "blob_slot" expects binary value, got int64`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			bindings := []SlotBinding{
				SlotValue(intSlot, int32(1)),
				SlotValue(floatSlot, 1.5),
				SlotValue(boolSlot, true),
				SlotValue(stringSlot, "text"),
				SlotValue(timeSlot, now),
				SlotValue(blobSlot, []byte("bytes")),
			}

			for i, binding := range bindings {
				if binding.slot == testCase.slot {
					bindings[i] = SlotValue(testCase.slot, testCase.value)
					break
				}
			}

			_, err := compiled.Bind(bindings...)
			require.EqualError(t, err, "jet: "+testCase.message)
		})
	}
}

func TestSlot_NullabilityIsEnforced(t *testing.T) {
	nonNullable := NewIntSlot("required")
	nullable := NewNullableIntSlot("optional")

	compiled, err := slotSelectStatement(defaultDialect, Eq(nonNullable, nullable)).Compile()
	require.NoError(t, err)

	_, err = compiled.Bind(
		SlotValue(nonNullable, nil),
		SlotValue(nullable, int64(1)),
	)
	require.EqualError(t, err, `jet: cannot bind NULL to non-nullable slot "required"`)

	bound, err := compiled.Bind(
		SlotValue(nonNullable, int64(1)),
		SlotValue(nullable, nil),
	)
	require.NoError(t, err)

	_, args := bound.Sql()
	require.Equal(t, []any{int64(1), nil}, args)
	require.Equal(t, "NULL", (&SQLBuilder{Dialect: defaultDialect, Debug: true}).argToString(nil))
}

func TestSlot_DriverValuerIsAcceptedAndNotEvaluatedEarly(t *testing.T) {
	slot := NewIntSlot("valuer_slot")
	compiled, err := slotSelectStatement(defaultDialect, table1Col1.EQ(slot)).Compile()
	require.NoError(t, err)

	valuer := &recordingValuer{value: int64(7)}

	require.Zero(t, valuer.callCount(), "Compile must not evaluate driver.Valuer")

	bound, err := compiled.Bind(SlotValue(slot, valuer))
	require.NoError(t, err)
	require.Zero(t, valuer.callCount(), "Bind must not evaluate driver.Valuer")

	_, args := bound.Sql()
	require.Same(t, valuer, args[0])
	require.Zero(t, valuer.callCount(), "Sql must not evaluate driver.Valuer")

	debugSQL := bound.DebugSql()
	require.Contains(t, debugSQL, "7")
	require.Equal(t, 1, valuer.callCount(), "DebugSql follows the existing debug stringification contract")
}

func TestSlot_ByteOwnershipIsolatedBetweenBinds(t *testing.T) {
	blob := NewBlobSlot("data")

	compiled, err := slotSelectStatement(defaultDialect, blob.EQ(blob)).Compile()
	require.NoError(t, err)

	source := []byte("hello")
	boundFirst, err := compiled.Bind(SlotValue(blob, source))
	require.NoError(t, err)

	_, argsFirst := boundFirst.Sql()
	require.Equal(t, []byte("hello"), argsFirst[0])

	// Mutating the caller's slice after Bind must not affect the bound arguments.
	source[0] = 'X'
	require.Equal(t, byte('h'), argsFirst[0].([]byte)[0])

	// A second Bind must not overwrite the arguments produced by the first one.
	boundSecond, err := compiled.Bind(SlotValue(blob, []byte("world")))
	require.NoError(t, err)

	_, argsSecond := boundSecond.Sql()
	require.Equal(t, "hello", string(argsFirst[0].([]byte)))
	require.Equal(t, "world", string(argsSecond[0].([]byte)))

	// The two bound argument slices must not share backing arrays.
	argsFirst[0].([]byte)[0] = 'A'
	require.Equal(t, "world", string(argsSecond[0].([]byte)))

	// Byte literals frozen at compile time are copied as well.
	constant := Literal([]byte("frozen"))
	compiledConstants, err := slotSelectStatement(defaultDialect, Eq(blob, constant)).Compile()
	require.NoError(t, err)

	literalBytes := constant.(*expression).Serializer.(*literalSerializer).value.([]byte)
	literalBytes[0] = 'Z'

	boundConstants, err := compiledConstants.Bind(SlotValue(blob, []byte("x")))
	require.NoError(t, err)

	_, constantArgs := boundConstants.Sql()
	require.Equal(t, []byte("x"), constantArgs[0])
	require.Equal(t, "frozen", string(constantArgs[1].([]byte)))
}

func TestSlot_ConcurrentBindIsRaceFreeAndIndependent(t *testing.T) {
	integerSlot := NewIntSlot("integer_slot")
	nullableString := NewNullableStringSlot("nullable_string")

	compiled, err := slotSelectStatement(defaultDialect, Eq(integerSlot, nullableString)).Compile()
	require.NoError(t, err)

	const goroutineCount = 200

	var waitGroup sync.WaitGroup

	waitGroup.Add(goroutineCount)

	for i := 0; i < goroutineCount; i++ {
		i := i

		go func() {
			defer waitGroup.Done()

			var stringValue any
			if i%2 == 0 {
				stringValue = "even"
			} // odd iterations bind NULL to the nullable slot

			bound, err := compiled.Bind(
				SlotValue(integerSlot, int64(i)),
				SlotValue(nullableString, stringValue),
			)
			require.NoError(t, err)

			_, args := bound.Sql()
			require.Len(t, args, 2)
			require.Equal(t, int64(i), args[0])

			if i%2 == 0 {
				require.Equal(t, "even", args[1])
			} else {
				require.Nil(t, args[1])
			}
		}()
	}

	waitGroup.Wait()
}

func TestSlot_SerializingWithoutCompilePanics(t *testing.T) {
	slot := NewIntSlot("oops")

	require.Panics(t, func() {
		slot.serialize(SelectStatementType, &SQLBuilder{Dialect: defaultDialect})
	})

	defer func() {
		recovered := recover()
		panicMessage, ok := recovered.(string)
		require.True(t, ok)
		require.Contains(t, panicMessage, `slot "oops"`)
		require.Contains(t, panicMessage, "Compile/Bind")
	}()

	slot.serialize(SelectStatementType, &SQLBuilder{Dialect: defaultDialect})
}

func TestSlot_RawNamedArgumentsKeepExistingContract(t *testing.T) {
	statement := slotSelectStatement(defaultDialect,
		RawBool("a = :value OR b = :value", map[string]any{":value": int64(7)}))

	compiled, err := statement.Compile()
	require.NoError(t, err)

	// Raw named arguments are ordinary constants for the structured slot machinery: the repeated
	// named argument reuses the placeholder in postgres dialects and needs no slot binding.
	bound, err := compiled.Bind()
	require.NoError(t, err)

	query, args := bound.Sql()
	require.Contains(t, query, "a = $1 OR b = $1")
	require.Equal(t, []any{int64(7)}, args)
	require.Empty(t, compiled.Slots())

	questionStatement := slotSelectStatement(questionDialect,
		RawBool("a = :value OR b = :value", map[string]any{":value": int64(7)}))

	questionCompiled, err := questionStatement.Compile()
	require.NoError(t, err)

	questionBound, err := questionCompiled.Bind()
	require.NoError(t, err)

	questionQuery, questionArgs := questionBound.Sql()
	require.Contains(t, questionQuery, "a = ? OR b = ?")
	require.Equal(t, []any{int64(7), int64(7)}, questionArgs)
}

func TestSlot_SlotAsProjectionAndOrderBy(t *testing.T) {
	slot := NewIntSlot("ordered")

	statement := newTestSelectStatement(defaultDialect,
		&ClauseSelect{ProjectionList: []Projection{slot, table1Col1}},
		&ClauseFrom{Tables: []Serializer{table1}},
		&ClauseOrderBy{List: []OrderByClause{slot.ASC()}},
	)

	compiled, err := statement.Compile()
	require.NoError(t, err)

	// The slot appears in the projection list and in ORDER BY; postgres reuses one placeholder.
	require.Equal(t, 2, strings.Count(compiled.Sql(), "$1"))

	bound, err := compiled.Bind(SlotValue(slot, int64(5)))
	require.NoError(t, err)

	query, args := bound.Sql()
	require.Contains(t, query, "ORDER BY")
	require.Equal(t, []any{int64(5)}, args)

	// Question dialects copy the argument for each occurrence.
	questionStatement := newTestSelectStatement(questionDialect,
		&ClauseSelect{ProjectionList: []Projection{slot, table1Col1}},
		&ClauseFrom{Tables: []Serializer{table1}},
		&ClauseOrderBy{List: []OrderByClause{slot.ASC()}},
	)

	questionCompiled, err := questionStatement.Compile()
	require.NoError(t, err)

	questionBound, err := questionCompiled.Bind(SlotValue(slot, int64(5)))
	require.NoError(t, err)

	_, questionArgs := questionBound.Sql()
	require.Equal(t, []any{int64(5), int64(5)}, questionArgs)
}

func TestSlot_FixedLiteralsAreInlinedAndOrdinarySqlIsUnchanged(t *testing.T) {
	slot := NewIntSlot("slot")

	compiled, err := slotSelectStatement(defaultDialect,
		Eq(FixedLiteral(int64(1)), slot)).Compile()
	require.NoError(t, err)

	bound, err := compiled.Bind(SlotValue(slot, int64(2)))
	require.NoError(t, err)

	query, args := bound.Sql()
	require.Contains(t, query, "1 = $1")
	require.Equal(t, []any{int64(2)}, args)

	// Statements without slots keep their existing Sql/DebugSql behavior.
	ordinary := slotSelectStatement(defaultDialect, table1Col1.EQ(Int(5)))

	ordinaryQuery, ordinaryArgs := ordinary.Sql()
	require.Contains(t, ordinaryQuery, "$1")
	require.Equal(t, []any{int64(5)}, ordinaryArgs)
	require.Contains(t, ordinary.DebugSql(), "= 5")
}

func TestSlot_BoundDebugSqlRendersAllPlaceholders(t *testing.T) {
	integerSlot := NewIntSlot("integer_slot")
	nullableSlot := NewNullableStringSlot("nullable_slot")

	compiled, err := slotSelectStatement(defaultDialect,
		Eq(integerSlot, nullableSlot)).Compile()
	require.NoError(t, err)

	bound, err := compiled.Bind(
		SlotValue(integerSlot, int64(5)),
		SlotValue(nullableSlot, nil),
	)
	require.NoError(t, err)

	require.Contains(t, bound.DebugSql(), "5 = NULL")

	questionCompiled, err := slotSelectStatement(questionDialect,
		Eq(integerSlot, NewIntSlot("other"))).Compile()
	require.NoError(t, err)

	questionBound, err := questionCompiled.Bind(
		SlotValue(integerSlot, int64(5)),
		SlotValue(questionCompiled.Slots()[1], int64(6)),
	)
	require.NoError(t, err)

	require.Contains(t, questionBound.DebugSql(), "5 = 6")
}

func TestSlot_BoundStatementCanBeRecompiled(t *testing.T) {
	slot := NewIntSlot("slot")

	compiled, err := slotSelectStatement(defaultDialect, table1Col1.EQ(slot)).Compile()
	require.NoError(t, err)

	bound, err := compiled.Bind(SlotValue(slot, int64(8)))
	require.NoError(t, err)

	recompiled, err := bound.Compile()
	require.NoError(t, err)
	require.Empty(t, recompiled.Slots())

	rebound, err := recompiled.Bind()
	require.NoError(t, err)

	reboundQuery, reboundArgs := rebound.Sql()
	boundQuery, boundArgs := bound.Sql()
	require.Equal(t, boundQuery, reboundQuery)
	require.Equal(t, boundArgs, reboundArgs)
	require.Equal(t, []any{int64(8)}, reboundArgs)

	_, err = recompiled.Bind(SlotValue(slot, int64(9)))
	require.EqualError(t, err, `jet: slot "slot" does not appear in the compiled statement`)
}

func TestSlot_ExecThroughDatabaseSQL(t *testing.T) {
	slot := NewIntSlot("id")

	compiled, err := slotSelectStatement(defaultDialect, table1Col1.EQ(slot)).Compile()
	require.NoError(t, err)

	db, err := sql.Open("jet_slot_fake_driver", "")
	require.NoError(t, err)
	defer db.Close()

	before := len(fakeDBDriver.snapshot())

	for _, value := range []int64{1, 2, 3} {
		bound, err := compiled.Bind(SlotValue(slot, value))
		require.NoError(t, err)

		_, err = bound.Exec(db)
		require.NoError(t, err)
	}

	executions := fakeDBDriver.snapshot()[before:]
	require.Len(t, executions, 3)

	for i, execution := range executions {
		require.Contains(t, execution.query, "$1")
		require.Equal(t, []driver.Value{int64(i + 1)}, execution.args)
	}

	// Rows goes through the same frozen query/argument pair.
	bound, err := compiled.Bind(SlotValue(slot, int64(4)))
	require.NoError(t, err)

	rows, err := bound.Rows(context.Background(), db)
	require.NoError(t, err)
	require.NoError(t, rows.Close())

	queryExecutions := fakeDBDriver.snapshot()[before+3:]
	require.Len(t, queryExecutions, 1)
	require.Equal(t, []driver.Value{int64(4)}, queryExecutions[0].args)

	// database/sql is what evaluates the Valuer at execution time.
	valuer := &recordingValuer{value: int64(9)}
	valuerBound, err := compiled.Bind(SlotValue(slot, valuer))
	require.NoError(t, err)
	require.Zero(t, valuer.callCount())

	_, err = valuerBound.Exec(db)
	require.NoError(t, err)
	require.GreaterOrEqual(t, valuer.callCount(), 1)
}

// --- recording driver.Valuer ---

type recordingValuer struct {
	mu    sync.Mutex
	calls int
	value driver.Value
}

func (r *recordingValuer) Value() (driver.Value, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls++
	return r.value, nil
}

func (r *recordingValuer) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.calls
}

// --- minimal database/sql/driver implementation for end-to-end execution tests ---

type fakeExecution struct {
	query string
	args  []driver.Value
}

type fakeDriverRecorder struct {
	mu         sync.Mutex
	executions []fakeExecution
}

func (r *fakeDriverRecorder) record(execution fakeExecution) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.executions = append(r.executions, execution)
}

func (r *fakeDriverRecorder) snapshot() []fakeExecution {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]fakeExecution(nil), r.executions...)
}

var fakeDBDriver = &fakeDriverRecorder{}

func init() {
	sql.Register("jet_slot_fake_driver", &fakeSQLDriver{recorder: fakeDBDriver})
}

type fakeSQLDriver struct {
	recorder *fakeDriverRecorder
}

func (d *fakeSQLDriver) Open(string) (driver.Conn, error) {
	return &fakeSQLConn{recorder: d.recorder}, nil
}

type fakeSQLConn struct {
	recorder *fakeDriverRecorder
}

func (c *fakeSQLConn) Prepare(query string) (driver.Stmt, error) {
	return &fakeSQLStmt{recorder: c.recorder, query: query}, nil
}

func (c *fakeSQLConn) Close() error { return nil }
func (c *fakeSQLConn) Begin() (driver.Tx, error) {
	return nil, errors.New("jet fake driver: transactions not supported")
}

type fakeSQLStmt struct {
	recorder *fakeDriverRecorder
	query    string
}

func (s *fakeSQLStmt) Close() error  { return nil }
func (s *fakeSQLStmt) NumInput() int { return -1 }

func (s *fakeSQLStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.recorder.record(fakeExecution{query: s.query, args: append([]driver.Value(nil), args...)})
	return fakeSQLResult{}, nil
}

func (s *fakeSQLStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.recorder.record(fakeExecution{query: s.query, args: append([]driver.Value(nil), args...)})
	return &fakeSQLRows{}, nil
}

type fakeSQLRows struct{}

func (r *fakeSQLRows) Columns() []string         { return []string{} }
func (r *fakeSQLRows) Close() error              { return nil }
func (r *fakeSQLRows) Next([]driver.Value) error { return io.EOF }

type fakeSQLResult struct{}

func (fakeSQLResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeSQLResult) RowsAffected() (int64, error) { return 1, nil }
