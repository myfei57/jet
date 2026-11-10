package postgres

import (
	"testing"

	"github.com/go-jet/jet/v2/internal/jet"
	"github.com/stretchr/testify/require"
)

func checkedOK(t *testing.T, statement jet.SerializerStatement) *jet.CheckedResult {
	t.Helper()

	result, err := Checked(statement)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, jet.StatusChecked, result.Report.Status())
	require.Empty(t, result.Report.Errors)
	require.Empty(t, result.Report.Opaque)

	sqlText, args := statement.Sql()
	require.Equal(t, sqlText, result.SQL)
	require.Equal(t, args, result.Args)

	return result
}

func checkedOKOpaque(t *testing.T, statement jet.SerializerStatement) *jet.CheckedResult {
	t.Helper()

	result, err := Checked(statement, WithOpaquePolicy(AcceptOpaque))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, jet.StatusOpaque, result.Report.Status())
	require.NotEmpty(t, result.Report.Opaque)

	return result
}

func checkedErr(t *testing.T, statement jet.SerializerStatement, options ...jet.CheckedOption) *jet.CheckedError {
	t.Helper()

	result, err := Checked(statement, options...)
	require.Error(t, err)
	require.Nil(t, result)

	var checkedErr *jet.CheckedError
	require.ErrorAs(t, err, &checkedErr)
	require.Equal(t, jet.StatusInvalid, checkedErr.Report.Status())
	require.NotEmpty(t, checkedErr.Report.Errors)

	return checkedErr
}

func requireErrorKind(t *testing.T, report *jet.CheckedReport, kind jet.ReferenceErrorKind) jet.ReferenceError {
	t.Helper()

	for _, refErr := range report.Errors {
		if refErr.Kind == kind {
			return refErr
		}
	}

	require.Failf(t, "reference error kind not found", "wanted %s, got %+v", kind, report.Errors)

	return jet.ReferenceError{}
}

func newCheckedTable(schema, name, alias string, columns ...jet.ColumnExpression) Table {
	return NewTable(schema, name, alias, columns...)
}

func TestChecked_SelectValidAndSqlParity(t *testing.T) {
	id := IntegerColumn("id")
	tbl := newCheckedTable("", "t1", "", id)

	stmt := SELECT(id).FROM(tbl)

	result := checkedOK(t, stmt)
	require.Contains(t, result.SQL, "FROM t1")

	// The compatibility entry must keep working unchanged after a checked run.
	sqlText, _ := stmt.Sql()
	require.Equal(t, sqlText, result.SQL)
}

func TestChecked_UnknownRelation(t *testing.T) {
	id := IntegerColumn("id")
	tbl := newCheckedTable("", "t1", "", id)

	ghostID := IntegerColumn("id")
	newCheckedTable("", "ghost", "", ghostID)

	stmt := SELECT(id).FROM(tbl).WHERE(ghostID.EQ(Int(1)))

	report := checkedErr(t, stmt).Report
	refErr := requireErrorKind(t, report, jet.ErrUnknownRelation)
	require.Equal(t, "ghost", refErr.Relation)
	require.Contains(t, refErr.Path, ".where")
}

func TestChecked_UnknownColumnOfKnownRelation(t *testing.T) {
	id := IntegerColumn("id")
	tbl := newCheckedTable("", "t1", "", id)

	missing := IntegerColumn("missing_col")
	jet.SetTableName(missing, "t1")

	stmt := SELECT(id).FROM(tbl).WHERE(missing.IS_NULL())

	report := checkedErr(t, stmt).Report
	refErr := requireErrorKind(t, report, jet.ErrUnknownColumn)
	require.Equal(t, "t1", refErr.Relation)
	require.Equal(t, "missing_col", refErr.Column)
}

func TestChecked_AmbiguousUnqualifiedColumn(t *testing.T) {
	aCol := IntegerColumn("id")
	a := newCheckedTable("", "t", "a", aCol)
	bCol := IntegerColumn("id")
	b := newCheckedTable("", "t", "b", bCol)

	loose := IntegerColumn("id") // no qualifier, matches both relations

	stmt := SELECT(aCol).FROM(a.INNER_JOIN(b, aCol.EQ(bCol))).WHERE(loose.GT(Int(0)))

	report := checkedErr(t, stmt).Report
	requireErrorKind(t, report, jet.ErrAmbiguousColumn)
}

func TestChecked_SamePhysicalTableDifferentAliasesAreDistinctRelations(t *testing.T) {
	leftID := IntegerColumn("id")
	left := newCheckedTable("", "t", "left_t", leftID)
	rightID := IntegerColumn("id")
	right := newCheckedTable("", "t", "right_t", rightID)

	stmt := SELECT(leftID, rightID).
		FROM(left.INNER_JOIN(right, leftID.EQ(rightID)))

	checkedOK(t, stmt)
}

func TestChecked_DuplicateAliasInSameBlock(t *testing.T) {
	firstID := IntegerColumn("id")
	first := newCheckedTable("", "t1", "dup", firstID)
	secondID := IntegerColumn("id")
	second := newCheckedTable("", "t2", "dup", secondID)

	stmt := SELECT(firstID).FROM(first, second)

	report := checkedErr(t, stmt).Report
	requireErrorKind(t, report, jet.ErrDuplicateRelation)
}

func TestChecked_AliasShadowingAcrossLayersIsLegal(t *testing.T) {
	outerCol := IntegerColumn("outer_id")
	outer := newCheckedTable("", "t1", "shadow", outerCol)

	innerCol := IntegerColumn("inner_id")
	inner := newCheckedTable("", "t2", "shadow", innerCol)

	// The inner derived table reuses the outer alias. Inside the subquery "shadow" resolves to the
	// inner relation, while the outer query keeps resolving to its own. Both layers are checked
	// independently.
	stmt := SELECT(outerCol).FROM(outer).WHERE(
		EXISTS(SELECT(innerCol).FROM(inner).WHERE(innerCol.GT(Int(0)))),
	)

	checkedOK(t, stmt)
}

func TestChecked_ShadowedOuterRelationIsNotReachableFromInnerScope(t *testing.T) {
	outerCol := IntegerColumn("outer_id")
	outer := newCheckedTable("", "t1", "shadow", outerCol)

	innerCol := IntegerColumn("inner_id")
	inner := newCheckedTable("", "t2", "shadow", innerCol)

	// outerCol renders as "shadow"."outer_id", but inside the subquery "shadow" means the inner
	// relation, which does not expose outer_id. Postgres would reject this too.
	stmt := SELECT(outerCol).FROM(outer).WHERE(
		EXISTS(SELECT(innerCol).FROM(inner).WHERE(outerCol.EQ(Int(2)))),
	)

	report := checkedErr(t, stmt).Report
	requireErrorKind(t, report, jet.ErrUnknownColumn)
}

func TestChecked_JoinOnSeesOnlyJoinSides(t *testing.T) {
	aCol := IntegerColumn("a_id")
	a := newCheckedTable("", "ta", "a", aCol)
	bCol := IntegerColumn("b_id")
	b := newCheckedTable("", "tb", "b", bCol)
	cCol := IntegerColumn("c_id")
	c := newCheckedTable("", "tc", "c", cCol)

	// Legal: the ON of the second join sees a, b and c.
	legal := SELECT(aCol).
		FROM(a.
			INNER_JOIN(b, aCol.EQ(bCol)).
			INNER_JOIN(c, bCol.EQ(cCol)))
	checkedOK(t, legal)

	// Illegal: the ON of the first join references c, which is not one of its two sides.
	illegal := SELECT(aCol).
		FROM(a.
			INNER_JOIN(b, aCol.EQ(cCol)).
			INNER_JOIN(c, bCol.EQ(cCol)))

	report := checkedErr(t, illegal).Report
	refErr := requireErrorKind(t, report, jet.ErrUnknownRelation)
	require.Equal(t, "c", refErr.Relation)
	require.Contains(t, refErr.Path, ".on")
}

func TestChecked_JoinOnCanCorrelateOuterQueryBlock(t *testing.T) {
	outerCol := IntegerColumn("outer_id")
	outer := newCheckedTable("", "outer_t", "o", outerCol)

	leftCol := IntegerColumn("left_id")
	left := newCheckedTable("", "left_t", "l", leftCol)
	rightCol := IntegerColumn("right_id")
	right := newCheckedTable("", "right_t", "r", rightCol)

	// The JOIN belongs to a subquery whose enclosing query block introduces o: the ON clause sees
	// both join sides and the legal outer scope, while same block siblings would stay hidden.
	subquery := SELECT(leftCol).
		FROM(left.INNER_JOIN(right, leftCol.EQ(rightCol).AND(rightCol.EQ(outerCol))))

	stmt := SELECT(outerCol).FROM(outer).WHERE(EXISTS(subquery))
	checkedOK(t, stmt)
}

func TestChecked_NonLateralDerivedTableCannotCorrelateSibling(t *testing.T) {
	outerCol := IntegerColumn("base_id")
	base := newCheckedTable("", "base", "b", outerCol)

	siblingCol := IntegerColumn("sibling_id")
	sibling := newCheckedTable("", "sibling", "s", siblingCol)

	derived := SELECT(siblingCol).FROM(sibling).WHERE(siblingCol.EQ(outerCol))

	stmt := SELECT(outerCol).FROM(base, derived.AsTable("d"))

	report := checkedErr(t, stmt).Report
	refErr := requireErrorKind(t, report, jet.ErrIllegalCorrelation)
	require.Equal(t, "b", refErr.Relation)
	require.Contains(t, refErr.Path, "from[1]")
	require.Contains(t, refErr.Path, ".where")
}

func TestChecked_DerivedTableCanCorrelateOuterQueryBlock(t *testing.T) {
	outerCol := IntegerColumn("outer_id")
	outer := newCheckedTable("", "outer_t", "o", outerCol)

	innerCol := IntegerColumn("inner_id")
	inner := newCheckedTable("", "inner_t", "i", innerCol)

	// The derived table belongs to the subquery's FROM, while the subquery itself is nested in the
	// outer query, so correlating with o is legal.
	scalar := SELECT(innerCol).FROM(inner).WHERE(innerCol.EQ(outerCol))
	stmt := SELECT(outerCol).FROM(outer).WHERE(scalar.IS_NOT_NULL())

	checkedOK(t, stmt)
}

func TestChecked_LateralCanReferenceLeftRelations(t *testing.T) {
	baseCol := IntegerColumn("base_id")
	base := newCheckedTable("", "base", "b", baseCol)

	sideCol := IntegerColumn("side_id")
	side := newCheckedTable("", "side", "s", sideCol)

	lateral := LATERAL(
		SELECT(sideCol).FROM(side).WHERE(sideCol.EQ(baseCol)),
	).AS("lat")

	stmt := SELECT(baseCol).FROM(base, lateral)

	checkedOK(t, stmt)
}

func TestChecked_LateralUnknownRelationIsRejected(t *testing.T) {
	baseCol := IntegerColumn("base_id")
	base := newCheckedTable("", "base", "b", baseCol)

	// A qualifier that is neither on the LATERAL's left side nor introduced inside its own body.
	phantomCol := IntegerColumn("ghost_id")
	jet.SetTableName(phantomCol, "phantom")

	lateral := LATERAL(
		SELECT(baseCol).WHERE(phantomCol.GT(Int(0))),
	).AS("lat")

	stmt := SELECT(baseCol).FROM(base, lateral)

	report := checkedErr(t, stmt).Report
	refErr := requireErrorKind(t, report, jet.ErrUnknownRelation)
	require.Equal(t, "phantom", refErr.Relation)
}

func TestChecked_ScalarSubqueryResolution(t *testing.T) {
	outerCol := IntegerColumn("outer_id")
	outer := newCheckedTable("", "outer_t", "o", outerCol)

	innerCol := IntegerColumn("inner_id")
	inner := newCheckedTable("", "inner_t", "i", innerCol)

	sub := SELECT(innerCol).FROM(inner).WHERE(innerCol.EQ(outerCol))

	legal := SELECT(outerCol).FROM(outer).WHERE(outerCol.IN(sub))
	checkedOK(t, legal)
}

func TestChecked_CTEVisibilityOrder(t *testing.T) {
	col1 := IntegerColumn("c1_col")
	t1 := newCheckedTable("", "t1", "", col1)
	col2 := IntegerColumn("c2_col")
	t2 := newCheckedTable("", "t2", "", col2)

	c1 := CTE("c1")
	c2 := CTE("c2")

	// c2 may reference c1 because c1 is introduced earlier in the WITH list.
	legal := WITH(
		c1.AS(SELECT(col1).FROM(t1)),
		c2.AS(SELECT(col2).FROM(t2).WHERE(col2.IN(SELECT(c1.AllColumns()).FROM(c1)))),
	)(
		SELECT(c1.AllColumns()).FROM(c1),
	)
	checkedOK(t, legal)

	// c1 cannot reference c2, which is introduced later. Give c2 explicit columns so its column
	// objects exist before its body is assigned.
	e2Col := IntegerColumn("c2_col")
	e1 := CTE("e1")
	e2 := CTE("e2", e2Col)

	illegal := WITH(
		e1.AS(SELECT(col1).FROM(t1).WHERE(col1.IN(SELECT(e2Col).FROM(e2)))),
		e2.AS(SELECT(col2).FROM(t2)),
	)(
		SELECT(col1).FROM(t1),
	)

	report := checkedErr(t, illegal).Report
	requireErrorKind(t, report, jet.ErrUnknownRelation)
}

func TestChecked_NonRecursiveCTECannotReferenceItself(t *testing.T) {
	col := IntegerColumn("n")
	tbl := newCheckedTable("", "numbers", "", col)

	self := CTE("self_cte", col)

	stmt := WITH(
		self.AS(SELECT(col).FROM(tbl).WHERE(col.IN(SELECT(self.AllColumns()).FROM(self)))),
	)(
		SELECT(self.AllColumns()).FROM(self),
	)

	report := checkedErr(t, stmt).Report
	requireErrorKind(t, report, jet.ErrUnknownRelation)
}

func TestChecked_RecursiveCTESelfReference(t *testing.T) {
	n1 := IntegerColumn("n")
	fib1 := IntegerColumn("fib")
	next1 := IntegerColumn("next_fib")
	fibonacci := CTE("fibonacci", IntegerColumn("n"), IntegerColumn("fib"), IntegerColumn("next_fib"))

	recursive := WITH_RECURSIVE(
		fibonacci.AS(
			SELECT(Int32(1), Int32(0), Int32(1)).UNION_ALL(
				SELECT(n1.ADD(Int(1)), next1, fib1.ADD(next1)).
					FROM(fibonacci).
					WHERE(n1.LT(Int(20))),
			),
		),
	)(
		SELECT(fibonacci.AllColumns()).FROM(fibonacci),
	)

	checkedOK(t, recursive)
}

func TestChecked_RecursiveCTENonRecursiveTermCannotSelfReference(t *testing.T) {
	n := IntegerColumn("n")
	bad := CTE("bad_rec", IntegerColumn("n"))

	// A single SELECT term is the non recursive term and must not self reference.
	stmt := WITH_RECURSIVE(
		bad.AS(SELECT(n).FROM(bad)),
	)(
		SELECT(bad.AllColumns()).FROM(bad),
	)

	report := checkedErr(t, stmt).Report
	requireErrorKind(t, report, jet.ErrUnknownRelation)
}

func TestChecked_DuplicateCTEName(t *testing.T) {
	col := IntegerColumn("c")
	tbl := newCheckedTable("", "t", "", col)

	first := CTE("dup_cte")
	second := CTE("dup_cte")

	stmt := WITH(
		first.AS(SELECT(col).FROM(tbl)),
		second.AS(SELECT(col).FROM(tbl)),
	)(
		SELECT(col).FROM(tbl),
	)

	report := checkedErr(t, stmt).Report
	requireErrorKind(t, report, jet.ErrDuplicateRelation)
}

func TestChecked_CTEShadowingOuterRelation(t *testing.T) {
	col := IntegerColumn("id")
	physical := newCheckedTable("", "shared_name", "", col)

	cteCol := IntegerColumn("id")
	cteTable := newCheckedTable("", "source_t", "", cteCol)
	shallow := CTE("shared_name")

	// The CTE name equals a physical table name. Inside the WITH scope the CTE wins, which is legal
	// shadowing and must not be reported as an error.
	stmt := WITH(
		shallow.AS(SELECT(cteCol).FROM(cteTable)),
	)(
		SELECT(shallow.AllColumns()).FROM(shallow),
	)
	checkedOK(t, stmt)

	// The physical table remains usable in a query block without the CTE in scope.
	checkedOK(t, SELECT(col).FROM(physical))
}

func TestChecked_InsertQuery(t *testing.T) {
	srcCol := IntegerColumn("id")
	src := newCheckedTable("", "src", "", srcCol)
	dstCol := IntegerColumn("id")
	dst := newCheckedTable("", "dst", "", dstCol)

	stmt := dst.INSERT(dstCol).QUERY(SELECT(srcCol).FROM(src))
	checkedOK(t, stmt)
}

func TestChecked_InsertQueryWithCTE(t *testing.T) {
	srcCol := IntegerColumn("id")
	src := newCheckedTable("", "src", "", srcCol)
	dstCol := IntegerColumn("id")
	dst := newCheckedTable("", "dst", "", dstCol)

	cte := CTE("stage")
	withStmt := WITH(
		cte.AS(SELECT(srcCol).FROM(src)),
	)(
		SELECT(cte.AllColumns()).FROM(cte),
	)

	stmt := dst.INSERT(dstCol).QUERY(withStmt)
	checkedOK(t, stmt)
}

func TestChecked_InsertQueryReferenceErrorHasQueryPath(t *testing.T) {
	dstCol := IntegerColumn("id")
	dst := newCheckedTable("", "dst", "", dstCol)

	srcCol := IntegerColumn("id")
	src := newCheckedTable("", "real_src", "rs", srcCol)

	ghostCol := IntegerColumn("id")
	newCheckedTable("", "ghost", "", ghostCol)

	// ghost is never introduced to the query scope.
	stmt := dst.INSERT(dstCol).QUERY(
		SELECT(srcCol).FROM(src).WHERE(ghostCol.GT(Int(0))),
	)

	report := checkedErr(t, stmt).Report
	refErr := requireErrorKind(t, report, jet.ErrUnknownRelation)
	require.Contains(t, refErr.Path, ".query")
}

func TestChecked_InsertReturning(t *testing.T) {
	id := IntegerColumn("id")
	tbl := newCheckedTable("", "t", "", id)

	checkedOK(t, tbl.INSERT(id).VALUES(Int(1)).RETURNING(id))

	missing := IntegerColumn("missing")
	jet.SetTableName(missing, "t")
	report := checkedErr(t, tbl.INSERT(id).VALUES(Int(1)).RETURNING(missing)).Report
	requireErrorKind(t, report, jet.ErrUnknownColumn)
}

func TestChecked_InsertOnConflictExcluded(t *testing.T) {
	id := IntegerColumn("id")
	name := StringColumn("name")
	tbl := newCheckedTable("", "t", "", id, name)

	// The generated EXCLUDED pseudo relation is a physical table named "excluded" with the same
	// output columns as the insert target.
	excludedName := StringColumn("name")
	newCheckedTable("", "excluded", "", excludedName)

	stmt := tbl.INSERT(id, name).
		VALUES(Int(1), String("a")).
		ON_CONFLICT(id).
		WHERE(id.GT(Int(0))).
		DO_UPDATE(SET(name.SET(excludedName)))

	checkedOK(t, stmt)

	// A relation other than the target and EXCLUDED cannot be reached from the conflict action.
	foreignCol := IntegerColumn("id")
	newCheckedTable("", "foreign_t", "f", foreignCol)
	bad := tbl.INSERT(id).
		VALUES(Int(1)).
		ON_CONFLICT(id).
		DO_UPDATE(SET(id.SET(foreignCol)))
	report := checkedErr(t, bad).Report
	requireErrorKind(t, report, jet.ErrUnknownRelation)
}

func TestChecked_InsertValuesLiterals(t *testing.T) {
	id := IntegerColumn("id")
	tbl := newCheckedTable("", "t", "", id)

	checkedOK(t, tbl.INSERT(id).VALUES(Int(42)))
}

func TestChecked_RawIsOpaqueAndRejectedByDefault(t *testing.T) {
	id := IntegerColumn("id")
	tbl := newCheckedTable("", "t", "", id)

	stmt := SELECT(id).FROM(tbl).WHERE(RawBool("id = 1"))

	result, err := Checked(stmt)
	require.Error(t, err)
	require.Nil(t, result)

	var checkedErr *jet.CheckedError
	require.ErrorAs(t, err, &checkedErr)
	require.Equal(t, jet.StatusInvalid, checkedErr.Report.Status())
	require.Len(t, checkedErr.Report.Opaque, 1)
	require.Equal(t, jet.OpaqueRaw, checkedErr.Report.Opaque[0].Kind)
	require.Contains(t, checkedErr.Report.Opaque[0].Path, ".where")
}

func TestChecked_RawAcceptedExplicitlyReturnsOpaqueStatus(t *testing.T) {
	id := IntegerColumn("id")
	tbl := newCheckedTable("", "t", "", id)

	stmt := SELECT(id).FROM(tbl).WHERE(RawBool("id = 1"))

	result := checkedOKOpaque(t, stmt)
	require.Contains(t, result.SQL, "FROM t")
}

func TestChecked_RawStatementIsOpaque(t *testing.T) {
	stmt := RawStatement("SELECT 1")

	result, err := Checked(stmt)
	require.Error(t, err)
	require.Nil(t, result)

	var checkedErr *jet.CheckedError
	require.ErrorAs(t, err, &checkedErr)
	require.Equal(t, jet.OpaqueRawStatement, checkedErr.Report.Opaque[0].Kind)

	accepted, acceptErr := Checked(stmt, WithOpaquePolicy(AcceptOpaque))
	require.NoError(t, acceptErr)
	require.Equal(t, jet.StatusOpaque, accepted.Report.Status())
}

func TestChecked_UnsupportedStatementRejected(t *testing.T) {
	id := IntegerColumn("id")
	tbl := newCheckedTable("", "t", "", id)

	stmt := tbl.UPDATE(id).SET(id.SET(Int(1)))

	report := checkedErr(t, stmt).Report
	requireErrorKind(t, report, jet.ErrUnsupportedStatement)
}

func TestChecked_DottedIdentifiersAreAtomic(t *testing.T) {
	weirdCol := IntegerColumn("Weird.Col")
	weirdTable := newCheckedTable("", "Weird.Table", "", weirdCol)

	stmt := SELECT(weirdCol).FROM(weirdTable)
	result := checkedOK(t, stmt)
	require.Contains(t, result.SQL, `"Weird.Table"."Weird.Col"`)

	// A qualified name containing a dot must not be split into a schema/table/column triple.
	missing := IntegerColumn("Missing.Col")
	jet.SetTableName(missing, "Weird.Table")
	report := checkedErr(t, SELECT(weirdCol).FROM(weirdTable).WHERE(missing.IS_NULL())).Report
	refErr := requireErrorKind(t, report, jet.ErrUnknownColumn)
	require.Equal(t, "Weird.Table", refErr.Relation)
	require.Equal(t, "Missing.Col", refErr.Column)
}

func TestChecked_DerivedTableColumns(t *testing.T) {
	baseCol := IntegerColumn("id")
	base := newCheckedTable("", "base", "", baseCol)

	derived := SELECT(baseCol).FROM(base).AsTable("d")
	derivedCols := derived.AllColumns()

	stmt := SELECT(derivedCols).FROM(derived)
	checkedOK(t, stmt)
}

func TestChecked_RepeatedRunsAreDeterministic(t *testing.T) {
	aCol := IntegerColumn("a_id")
	a := newCheckedTable("", "ta", "a", aCol)
	bCol := IntegerColumn("b_id")
	b := newCheckedTable("", "tb", "b", bCol)

	stmt := SELECT(aCol, bCol).FROM(a.INNER_JOIN(b, aCol.EQ(bCol))).ORDER_BY(aCol.DESC())

	first := checkedOK(t, stmt)
	second := checkedOK(t, stmt)

	require.Equal(t, first.SQL, second.SQL)
	require.Equal(t, first.Args, second.Args)

	// Error reports must be stable across runs as well.
	ghostCol := IntegerColumn("id")
	newCheckedTable("", "ghost", "", ghostCol)
	badStmt := SELECT(aCol).FROM(a).WHERE(ghostCol.EQ(Int(1)))

	firstErr := checkedErr(t, badStmt).Error()
	secondErr := checkedErr(t, badStmt).Error()
	require.Equal(t, firstErr, secondErr)
}

func TestChecked_CrossJoin(t *testing.T) {
	aCol := IntegerColumn("a_id")
	a := newCheckedTable("", "ta", "a", aCol)
	bCol := IntegerColumn("b_id")
	b := newCheckedTable("", "tb", "b", bCol)

	stmt := SELECT(aCol, bCol).FROM(a.CROSS_JOIN(b))
	checkedOK(t, stmt)
}

func TestChecked_ValuesDerivedTable(t *testing.T) {
	v1 := IntegerColumn("column1")
	v2 := StringColumn("column2")
	valuesTable := VALUES(WRAP(Int32(1), String("x"))).AS("v", v1, v2)

	stmt := SELECT(valuesTable.AllColumns()).FROM(valuesTable)
	checkedOK(t, stmt)

	missing := IntegerColumn("column3")
	jet.SetTableName(missing, "v")
	report := checkedErr(t, SELECT(v1).FROM(valuesTable).WHERE(missing.IS_NULL())).Report
	requireErrorKind(t, report, jet.ErrUnknownColumn)
}

func TestChecked_SetStatementRoot(t *testing.T) {
	aCol := IntegerColumn("a_id")
	a := newCheckedTable("", "ta", "a", aCol)
	bCol := IntegerColumn("b_id")
	b := newCheckedTable("", "tb", "b", bCol)

	set := SELECT(aCol).FROM(a).UNION_ALL(SELECT(bCol).FROM(b)).ORDER_BY(aCol.ASC())
	checkedOK(t, set)
}

func TestChecked_SelectJson(t *testing.T) {
	aCol := IntegerColumn("a_id")
	a := newCheckedTable("", "ta", "a", aCol)

	stmt := SELECT_JSON_ARR(aCol).FROM(a)
	checkedOK(t, stmt)
}

func TestChecked_WindowFunction(t *testing.T) {
	aCol := IntegerColumn("a_id")
	aName := StringColumn("name")
	a := newCheckedTable("", "ta", "a", aCol, aName)

	stmt := SELECT(COUNT(aCol).OVER(PARTITION_BY(aName))).FROM(a)
	checkedOK(t, stmt)
}

func TestChecked_WithWrappingInsert(t *testing.T) {
	srcCol := IntegerColumn("id")
	src := newCheckedTable("", "src", "", srcCol)
	dstCol := IntegerColumn("id")
	dst := newCheckedTable("", "dst", "", dstCol)

	stage := CTE("stage")
	stmt := WITH(
		stage.AS(SELECT(srcCol).FROM(src)),
	)(
		dst.INSERT(dstCol).QUERY(SELECT(stage.AllColumns()).FROM(stage)),
	)

	checkedOK(t, stmt)
}

func TestChecked_CTEAliasAndNestedDerivedTables(t *testing.T) {
	col1 := IntegerColumn("c1_col")
	t1 := newCheckedTable("", "t1", "", col1)
	col2 := IntegerColumn("c2_col")
	t2 := newCheckedTable("", "t2", "", col2)

	c1 := CTE("c1")
	c2 := c1.ALIAS("c2")

	// The same CTE used twice, once directly and once through an explicit alias.
	stmt := WITH(
		c1.AS(SELECT(col1).FROM(t1)),
	)(
		SELECT(c1.AllColumns().As("c1.*"), c2.AllColumns().As("c2.*")).
			FROM(c1.INNER_JOIN(c2, Bool(true))),
	)
	checkedOK(t, stmt)

	// A regular derived table nested inside another derived table.
	inner := SELECT(col2).FROM(t2).AsTable("inner")
	outer := SELECT(inner.AllColumns()).FROM(inner).AsTable("outer")
	outerCols := outer.AllColumns()
	checkedOK(t, SELECT(outerCols).FROM(outer))
}

func TestChecked_RawMixedWithValidReferences(t *testing.T) {
	id := IntegerColumn("id")
	tbl := newCheckedTable("", "t", "", id)

	// The structured part is valid, but the Raw fragment keeps the report opaque.
	stmt := SELECT(id).FROM(tbl).WHERE(id.GT(Int(0)).AND(RawBool("id % 2 = 0")))

	_, err := Checked(stmt)
	require.Error(t, err)

	accepted, acceptErr := Checked(stmt, WithOpaquePolicy(AcceptOpaque))
	require.NoError(t, acceptErr)
	require.Equal(t, jet.StatusOpaque, accepted.Report.Status())
	require.Len(t, accepted.Report.Opaque, 1)
	require.Contains(t, accepted.Report.Opaque[0].Path, ".where")
	require.Contains(t, accepted.SQL, "id % 2 = 0")
}

func TestChecked_DoesNotMutateStatement(t *testing.T) {
	id := IntegerColumn("id")
	tbl := newCheckedTable("", "t", "", id)

	stmt := SELECT(id).FROM(tbl)
	beforeSQL, beforeArgs := stmt.Sql()

	_, err := Checked(stmt)
	require.NoError(t, err)

	afterSQL, afterArgs := stmt.Sql()
	require.Equal(t, beforeSQL, afterSQL)
	require.Equal(t, beforeArgs, afterArgs)
}
