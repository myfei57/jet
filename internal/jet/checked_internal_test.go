package jet

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func newTestChecker() *referenceChecker {
	return newReferenceChecker()
}

func scopeWithRelation(parent *refScope, name string, columns ...string) *refScope {
	scope := newScope(parent)

	columnSet := map[string]struct{}{}
	for _, column := range columns {
		columnSet[column] = struct{}{}
	}

	scope.add(&relation{name: name, columns: columnSet, path: "test." + name}, &CheckedReport{}, "test."+name)

	return scope
}

func TestCheckedScope_OutputAliasVisibilityRules(t *testing.T) {
	block := scopeWithRelation(nil, "t", "id")
	block.outputs = map[string]struct{}{"total": {}}

	// ORDER BY: output alias wins and is visible.
	orderChecker := newTestChecker()
	orderChecker.resolveColumn(columnRefInfo{column: "total"}, block, "p.order", outputsOutputFirst)
	require.Empty(t, orderChecker.report.Errors)

	// GROUP BY: input columns take precedence, but an output alias resolves when no input column
	// carries the name.
	groupChecker := newTestChecker()
	groupChecker.resolveColumn(columnRefInfo{column: "total"}, block, "p.group", outputsInputFirst)
	require.Empty(t, groupChecker.report.Errors)

	// WHERE/HAVING/ON: output aliases are never visible.
	hiddenChecker := newTestChecker()
	hiddenChecker.resolveColumn(columnRefInfo{column: "total"}, block, "p.where", outputsHidden)
	require.Len(t, hiddenChecker.report.Errors, 1)
	require.Equal(t, ErrUnknownColumn, hiddenChecker.report.Errors[0].Kind)

	// A qualified reference can never resolve through an output alias, even in ORDER BY.
	qualifiedChecker := newTestChecker()
	qualifiedChecker.resolveColumn(columnRefInfo{qualifier: "t", column: "total"}, block, "p.order", outputsOutputFirst)
	require.Len(t, qualifiedChecker.report.Errors, 1)
	require.Equal(t, ErrUnknownColumn, qualifiedChecker.report.Errors[0].Kind)

	// Input columns remain visible in every mode.
	for _, mode := range []outputResolution{outputsHidden, outputsInputFirst, outputsOutputFirst} {
		checker := newTestChecker()
		checker.resolveColumn(columnRefInfo{column: "id"}, block, "p", mode)
		require.Empty(t, checker.report.Errors)
	}
}

func TestCheckedScope_GroupByInputPrecedence(t *testing.T) {
	// An output alias that has the same name as an input column resolves to the input relation in
	// GROUP BY, while ORDER BY would prefer the output alias. Both must succeed here because the
	// name exists in both namespaces.
	block := scopeWithRelation(nil, "t", "name")
	block.outputs = map[string]struct{}{"name": {}}

	groupChecker := newTestChecker()
	groupChecker.resolveColumn(columnRefInfo{column: "name"}, block, "p.group", outputsInputFirst)
	require.Empty(t, groupChecker.report.Errors)
}

func TestCheckedScope_ShadowingAndDuplication(t *testing.T) {
	report := &CheckedReport{}

	outer := newScope(nil)
	outer.add(&relation{name: "shared", columns: map[string]struct{}{"outer_col": {}}, path: "outer"}, report, "outer")
	require.Empty(t, report.Errors)

	// Reusing the name in a nested block is legal shadowing, not a duplicate.
	inner := newScope(outer)
	inner.add(&relation{name: "shared", columns: map[string]struct{}{"inner_col": {}}, path: "inner"}, report, "inner")
	require.Empty(t, report.Errors)

	// Resolution inside the inner block picks the inner relation.
	_, rel := inner.findRelation("shared")
	require.Equal(t, "inner", rel.path)
	_, ok := rel.columns["inner_col"]
	require.True(t, ok)

	// Resolution from the outer block keeps picking the outer relation.
	_, outerRel := outer.findRelation("shared")
	require.Equal(t, "outer", outerRel.path)

	// Introducing the same name twice in one block is a duplicate relation error.
	duplicateChecker := newTestChecker()
	block := newScope(nil)
	block.add(&relation{name: "dup", path: "first"}, duplicateChecker.report, "first")
	block.add(&relation{name: "dup", path: "second"}, duplicateChecker.report, "second")
	require.Len(t, duplicateChecker.report.Errors, 1)
	require.Equal(t, ErrDuplicateRelation, duplicateChecker.report.Errors[0].Kind)
}

func TestCheckedScope_UnknownRelationWithAvailableNames(t *testing.T) {
	block := scopeWithRelation(nil, "a", "id")
	scopeWithRelation(block, "b", "id")

	checker := newTestChecker()
	checker.resolveColumn(columnRefInfo{qualifier: "missing", column: "id"}, block, "p.where", outputsHidden)

	require.Len(t, checker.report.Errors, 1)
	refErr := checker.report.Errors[0]
	require.Equal(t, ErrUnknownRelation, refErr.Kind)
	require.Equal(t, "missing", refErr.Relation)
	require.Contains(t, refErr.Message, "a")
	require.Contains(t, refErr.Message, "b")
}

func TestCheckedScope_UnknownColumnSetIsUnprovableNotRejected(t *testing.T) {
	block := newScope(nil)
	block.add(&relation{name: "opaque_rel", columns: nil, path: "p.rel"}, &CheckedReport{}, "p.rel")

	checker := newTestChecker()
	checker.resolveColumn(columnRefInfo{qualifier: "opaque_rel", column: "anything"}, block, "p.where", outputsHidden)
	require.Empty(t, checker.report.Errors)

	checker.resolveColumn(columnRefInfo{column: "anything"}, block, "p.where", outputsHidden)
	require.Empty(t, checker.report.Errors)
}

// mysterySerializer is a custom, non traversable serializer: the checker must classify it as
// opaque instead of silently ignoring its contents.
type mysterySerializer struct{}

func (m *mysterySerializer) serialize(statement StatementType, out *SQLBuilder, options ...SerializeOption) {
	out.WriteString("MYSTERY()")
}

func TestCheckedWalk_CustomSerializerIsOpaque(t *testing.T) {
	checker := newTestChecker()
	checker.checkExpression(&mysterySerializer{}, newScope(nil), "p.custom")

	require.Empty(t, checker.report.Errors)
	require.Len(t, checker.report.Opaque, 1)
	require.Equal(t, OpaqueCustom, checker.report.Opaque[0].Kind)
	require.Equal(t, "p.custom", checker.report.Opaque[0].Path)
}

func TestCheckedWalk_RawExpressionIsOpaque(t *testing.T) {
	checker := newTestChecker()
	checker.checkExpression(Raw("some_raw_fragment()"), newScope(nil), "p.raw")

	require.Len(t, checker.report.Opaque, 1)
	require.Equal(t, OpaqueRaw, checker.report.Opaque[0].Kind)
}

func TestCheckedColumnRefInfo_KeepsDottedIdentifiersAtomic(t *testing.T) {
	column := NewColumnImpl("Weird.Col", "Weird.Table", nil)

	info := column.columnRefInfo()
	require.Equal(t, "Weird.Table", info.qualifier)
	require.Equal(t, "Weird.Col", info.column)

	// Subquery output names may contain dots as well, and they stay atomic.
	selectTable := NewSelectTable(nil, "derived", []ColumnExpression{column})
	subQueryColumn := IntegerColumn("Weird.Output")
	subQueryColumn.setSubQuery(selectTable)

	subInfo := subQueryColumn.(columnRefProvider).columnRefInfo()
	require.Equal(t, "derived", subInfo.qualifier)
	require.Equal(t, "Weird.Output", subInfo.column)
}

func TestCheckedColumnRefInfo_TableAndAliasAreDistinctIdentities(t *testing.T) {
	first := NewColumnImpl("id", "left_t", nil)
	second := NewColumnImpl("id", "right_t", nil)

	firstInfo := first.columnRefInfo()
	secondInfo := second.columnRefInfo()

	require.Equal(t, "left_t", firstInfo.qualifier)
	require.Equal(t, "right_t", secondInfo.qualifier)
	require.NotEqual(t, firstInfo.qualifier, secondInfo.qualifier)
}
