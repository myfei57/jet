package jet

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type userSerializer struct{}

func (userSerializer) serialize(statement StatementType, out *SQLBuilder, options ...SerializeOption) {
	out.WriteString("user_defined_fragment()")
}

func TestInspectSerializerSafeExpressions(t *testing.T) {
	volatile := map[string]bool{"RANDOM": true}

	safeExpressions := []Serializer{
		Literal(1),
		FixedLiteral(2),
		Keyword("DEFAULT"),
		table1Col1,
		COALESCE(Int(1), Int(2)),
		table1Col1.ADD(Int(3)),
		Int(1).BETWEEN(Int(0), Int(10)),
		CASE().WHEN(Bool(true)).THEN(Int(1)).ELSE(Int(2)),
		Func("ABS", Int(4)),
		ROW(defaultDialect, Int(1), String("x")),
		COUNT(table1Col1).OVER(PARTITION_BY(table1ColBool)),
		NewBinaryOperatorExpression(Int(1), Int(2), "+"),
	}

	for _, expression := range safeExpressions {
		inspection := InspectSerializer(expression, volatile)
		require.False(t, inspection.RequiresExplicitOptIn(), "expression should be safe: %#v", expression)
		require.Empty(t, inspection.VolatileFunctions)
	}
}

func TestInspectSerializerOpaqueAndVolatile(t *testing.T) {
	volatile := map[string]bool{"RANDOM": true, "NEXTVAL": true}

	// Raw fragments are opaque wherever they are nested.
	require.True(t, InspectSerializer(Raw("nextval('s')"), volatile).HasOpaque)

	insideCase := CASE().WHEN(Bool(true)).THEN(Int(1)).ELSE(Raw("random()"))
	require.True(t, InspectSerializer(insideCase, volatile).HasOpaque)

	insideRow := ROW(defaultDialect, Int(1), Raw("side_effect()"))
	require.True(t, InspectSerializer(insideRow, volatile).HasOpaque)

	// Unknown user-provided serializers are treated as opaque.
	require.True(t, InspectSerializer(userSerializer{}, volatile).HasOpaque)

	// Volatile function names are reported and are still discovered through nested expressions.
	inspection := InspectSerializer(Func("random"), volatile)
	require.Equal(t, []string{"RANDOM"}, inspection.VolatileFunctions)
	require.True(t, inspection.RequiresExplicitOptIn())

	nested := COALESCE(Int(1), Func("nextval", String("s")))
	inspection = InspectSerializer(nested, volatile)
	require.Equal(t, []string{"NEXTVAL"}, inspection.VolatileFunctions)

	// Without registering the name, a regular function is safe.
	require.False(t, InspectSerializer(Func("abs", Int(1)), volatile).RequiresExplicitOptIn())
}

func TestInspectSerializerStatements(t *testing.T) {
	volatile := map[string]bool{}

	inspection := InspectSerializer(RawStatement(defaultDialect, "SELECT 1"), volatile)
	require.True(t, inspection.HasStatement)
	require.True(t, inspection.RequiresExplicitOptIn())

	// Tables and select tables are statement-dependent structures.
	require.True(t, InspectSerializer(table1, volatile).HasStatement)
}

func TestInspectProjection(t *testing.T) {
	volatile := map[string]bool{"RANDOM": true}

	// Aliased expressions are unwrapped.
	require.False(t, InspectProjection(Int(1).AS("answer"), volatile).RequiresExplicitOptIn())

	aliasedRaw := RawInt("1").AS("computed")
	require.True(t, InspectProjection(aliasedRaw, volatile).HasOpaque)

	// Projection lists are traversed element by element.
	list := ProjectionList{table1Col1, table1ColFloat, Func("RANDOM")}
	inspection := InspectProjection(list, volatile)
	require.Equal(t, []string{"RANDOM"}, inspection.VolatileFunctions)

	// Column lists are reachable as serializers.
	require.False(t, InspectProjection(ColumnList{table1Col1, table1ColInt}, volatile).RequiresExplicitOptIn())
}

func TestLiteralValue(t *testing.T) {
	value, ok := LiteralValue(Literal(42))
	require.True(t, ok)
	require.Equal(t, 42, value)

	// Wrapped literals (the common VALUES/MODEL form) are unwrapped.
	value, ok = LiteralValue(Int(7))
	require.True(t, ok)
	require.Equal(t, int64(7), value)

	// Fixed literals are inlined and cannot be compared as bound values.
	_, ok = LiteralValue(FixedLiteral(7))
	require.False(t, ok)

	// Keywords such as DEFAULT and NULL are not bound literals.
	_, ok = LiteralValue(DEFAULT)
	require.False(t, ok)
	_, ok = LiteralValue(NULL)
	require.False(t, ok)

	// Columns and arbitrary expressions expose no literal value.
	_, ok = LiteralValue(table1Col1)
	require.False(t, ok)
	_, ok = LiteralValue(COALESCE(Int(1), Int(2)))
	require.False(t, ok)

	_, ok = LiteralValue(nil)
	require.False(t, ok)
}
