package jet

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/go-jet/jet/v2/internal/utils/is"
)

// Boundary labels stored on scopes to explain why a relation name that exists in the same query
// block is not visible from a reference location.
const (
	boundaryCorrelation = "correlation"
	boundaryLateral     = "lateral"
	boundaryJoinOn      = "join_on"
)

// outputResolution controls how SELECT output aliases participate in name resolution.
type outputResolution int

const (
	// outputsHidden is used in WHERE, HAVING, JOIN ON and similar clauses: output aliases defined in
	// the SELECT list are not visible at all.
	outputsHidden outputResolution = iota
	// outputsInputFirst is used in GROUP BY: output aliases are visible only when the name does not
	// resolve to an input column first.
	outputsInputFirst
	// outputsOutputFirst is used in ORDER BY: a matching output alias wins over input columns.
	outputsOutputFirst
)

type referenceChecker struct {
	report *CheckedReport
}

func newReferenceChecker() *referenceChecker {
	return &referenceChecker{report: &CheckedReport{}}
}

// relation is a single visible FROM entry.
type relation struct {
	// name is the effective relation identifier: alias when present, table name otherwise. The
	// value is atomic, it is never split on dots nor case folded.
	name string
	// columns holds the known output column names of the relation. A nil map means the column set
	// is not statically known, in which case column membership is left unproven.
	columns map[string]struct{}
	// path is the structured location where the relation was introduced.
	path string
	// origin describes the provenance of the relation (physical table, derived table, CTE, ...).
	origin string
}

// refScope is one lexical block in the static scope chain.
type refScope struct {
	parent *refScope
	rels   []*relation
	byName map[string]*relation
	// outputs holds the SELECT list output aliases/names visible from this scope. It is only
	// populated for the ORDER BY/GROUP BY phases of a query block.
	outputs map[string]struct{}
	// hidden maps relation names that exist in the same query block but are not reachable from the
	// current scope to the boundary that hides them.
	hidden map[string]string
}

func newScope(parent *refScope) *refScope {
	return &refScope{
		parent: parent,
		byName: map[string]*relation{},
		hidden: map[string]string{},
	}
}

func (s *refScope) add(rel *relation, report *CheckedReport, path string) {
	if rel.name == "" {
		s.rels = append(s.rels, rel)
		return
	}

	if existing, duplicated := s.byName[rel.name]; duplicated {
		report.Errors = append(report.Errors, ReferenceError{
			Kind:     ErrDuplicateRelation,
			Path:     path,
			Relation: rel.name,
			Message: fmt.Sprintf(
				"relation name %q is introduced more than once in the same query block (first introduced at %s)",
				rel.name, existing.path),
		})
		return
	}

	s.rels = append(s.rels, rel)
	s.byName[rel.name] = rel
}

func (s *refScope) findRelation(name string) (*refScope, *relation) {
	for scope := s; scope != nil; scope = scope.parent {
		if rel, ok := scope.byName[name]; ok {
			return scope, rel
		}
	}

	return nil, nil
}

func (s *refScope) findHidden(name string) string {
	for scope := s; scope != nil; scope = scope.parent {
		if boundary, ok := scope.hidden[name]; ok {
			return boundary
		}
	}

	return ""
}

// findRelationInChain reports whether name is introduced in the scope or any of its parents.
func findRelationInChain(scope *refScope, name string) (*refScope, *relation) {
	if scope == nil {
		return nil, nil
	}

	return scope.findRelation(name)
}

// availableRelationNames collects relation names visible from the scope, in deterministic scope
// and introduction order, for diagnostic messages.
func (s *refScope) availableRelationNames() []string {
	seen := map[string]bool{}

	var names []string

	for scope := s; scope != nil; scope = scope.parent {
		for _, rel := range scope.rels {
			if rel.name != "" && !seen[rel.name] {
				seen[rel.name] = true
				names = append(names, rel.name)
			}
		}
	}

	return names
}

func (c *referenceChecker) addError(path string, kind ReferenceErrorKind, relationName, column, message string) {
	c.report.Errors = append(c.report.Errors, ReferenceError{
		Kind:     kind,
		Path:     path,
		Relation: relationName,
		Column:   column,
		Message:  message,
	})
}

func (c *referenceChecker) addOpaque(path string, kind OpaqueKind, detail string) {
	c.report.Opaque = append(c.report.Opaque, OpaqueFinding{
		Path:   path,
		Kind:   kind,
		Detail: detail,
	})
}

func indexPath(path, slot string, index int) string {
	return path + "." + slot + "[" + strconv.Itoa(index) + "]"
}

// ---------------------------------------------------------------------
// Root dispatch
// ---------------------------------------------------------------------

func (c *referenceChecker) checkRoot(statement SerializerStatement) *CheckedReport {
	c.checkStatement(statement, nil, "statement")
	return c.report
}

func (c *referenceChecker) checkStatement(node Serializer, outer *refScope, path string) {
	if is.Nil(node) {
		return
	}

	node = checkedUnwrap(node)
	if is.Nil(node) {
		return
	}

	switch stmt := node.(type) {
	case *withImpl:
		c.checkWith(stmt, outer, path)
	case *expressionStatementImpl:
		c.checkExpressionStatement(stmt, outer, path)
	case *statementImpl:
		c.checkPlainStatement(stmt, outer, path)
	case *rawStatementImpl:
		c.addOpaque(path, OpaqueRawStatement, "RawStatement content is an opaque SQL string")
	case SerializerStatement:
		c.addOpaque(path, OpaqueCustom, fmt.Sprintf("statement type %T cannot be traversed by the checker", node))
	default:
		c.checkExpression(node, outer, path)
	}
}

func (c *referenceChecker) checkPlainStatement(stmt *statementImpl, outer *refScope, path string) {
	switch stmt.statementType {
	case InsertStatementType:
		c.checkInsert(stmt, outer, path)
	default:
		c.addError(path, ErrUnsupportedStatement, "", "",
			fmt.Sprintf("statement type %q is outside the checked serialization range (SELECT, WITH and INSERT are supported)",
				string(stmt.statementType)))
	}
}

// ---------------------------------------------------------------------
// SELECT and set operation query blocks
// ---------------------------------------------------------------------

type selectClauses struct {
	selectClause *ClauseSelect
	fromClause   *ClauseFrom
	whereClause  *ClauseWhere
	groupBy      *ClauseGroupBy
	having       *ClauseHaving
	window       *ClauseWindow
	orderBy      *ClauseOrderBy
	offset       *ClauseOffset
	fetch        *ClauseFetch
	forClause    *ClauseFor
	setOperator  *ClauseSetStmtOperator
}

func extractSelectClauses(clauses []Clause) selectClauses {
	var parsed selectClauses

	for _, clause := range clauses {
		switch cl := clause.(type) {
		case *ClauseSelect:
			parsed.selectClause = cl
		case *ClauseFrom:
			parsed.fromClause = cl
		case *ClauseWhere:
			parsed.whereClause = cl
		case *ClauseGroupBy:
			parsed.groupBy = cl
		case *ClauseHaving:
			parsed.having = cl
		case *ClauseWindow:
			parsed.window = cl
		case *ClauseOrderBy:
			parsed.orderBy = cl
		case *ClauseOffset:
			parsed.offset = cl
		case *ClauseFetch:
			parsed.fetch = cl
		case *ClauseFor:
			parsed.forClause = cl
		case *ClauseSetStmtOperator:
			parsed.setOperator = cl
		}
	}

	return parsed
}

func (c *referenceChecker) checkExpressionStatement(stmt *expressionStatementImpl, outer *refScope, path string) {
	parsed := extractSelectClauses(stmt.Clauses)

	if parsed.setOperator != nil {
		c.checkSetOperator(parsed.setOperator, outer, path)
		return
	}

	block := newScope(outer)

	if parsed.fromClause != nil {
		c.checkFromItems(parsed.fromClause.Tables, block, outer, path)
	}

	if parsed.whereClause != nil && !is.Nil(parsed.whereClause.Condition) {
		c.checkExpression(parsed.whereClause.Condition, block, path+".where")
	}

	outputs := map[string]struct{}{}

	if parsed.selectClause != nil {
		for i, projection := range parsed.selectClause.ProjectionList {
			c.checkProjection(projection, block, indexPath(path, "projections", i), outputs)
		}

		for i, column := range parsed.selectClause.DistinctOnColumns {
			c.checkExpression(column, block, indexPath(path, "distinct_on", i))
		}
	}

	if parsed.groupBy != nil {
		groupScope := c.scopeWithOutputs(block, outputs)
		for i, groupClause := range parsed.groupBy.List {
			if is.Nil(groupClause) {
				continue
			}
			c.checkGroupByClause(groupClause, groupScope, indexPath(path, "group_by", i))
		}
	}

	if parsed.having != nil && !is.Nil(parsed.having.Condition) {
		c.checkExpression(parsed.having.Condition, block, path+".having")
	}

	if parsed.window != nil {
		for i, definition := range parsed.window.Definitions {
			if is.Nil(definition.Window) {
				continue
			}
			c.checkExpression(definition.Window, block, indexPath(path, "window", i))
		}
	}

	if parsed.orderBy != nil {
		orderScope := c.scopeWithOutputs(block, outputs)
		for i, orderClause := range parsed.orderBy.List {
			if is.Nil(orderClause) {
				continue
			}
			c.checkOrderByClause(orderClause, orderScope, indexPath(path, "order_by", i))
		}
	}

	if parsed.offset != nil && !is.Nil(parsed.offset.Count) {
		c.checkExpression(parsed.offset.Count, block, path+".offset")
	}

	if parsed.fetch != nil && !is.Nil(parsed.fetch.Count) {
		c.checkExpression(parsed.fetch.Count, block, path+".fetch")
	}

	if parsed.forClause != nil {
		for i, lock := range parsed.forClause.Locks {
			c.checkRowLock(lock, block, indexPath(path, "for", i))
		}
	}
}

func (c *referenceChecker) scopeWithOutputs(block *refScope, outputs map[string]struct{}) *refScope {
	if len(outputs) == 0 {
		return block
	}

	clone := newScope(block.parent)
	clone.rels = block.rels
	clone.byName = block.byName
	clone.hidden = block.hidden
	clone.outputs = outputs

	return clone
}

func (c *referenceChecker) checkSetOperator(setOperator *ClauseSetStmtOperator, outer *refScope, path string) {
	for i, selectStmt := range setOperator.Selects {
		c.checkStatement(selectStmt, outer, indexPath(path, "arms", i))
	}

	outputs := map[string]struct{}{}

	if len(setOperator.Selects) > 0 && !is.Nil(setOperator.Selects[0]) {
		if first, ok := checkedUnwrap(setOperator.Selects[0]).(HasProjections); ok {
			c.collectOutputs(first.projections(), outputs)
		}
	}

	orderScope := c.scopeWithOutputs(newScope(outer), outputs)

	for i, orderClause := range setOperator.OrderBy.List {
		if is.Nil(orderClause) {
			continue
		}
		c.checkOrderByClause(orderClause, orderScope, indexPath(path, "order_by", i))
	}

	if !is.Nil(setOperator.Offset.Count) {
		c.checkExpression(setOperator.Offset.Count, newScope(outer), path+".offset")
	}
}

func (c *referenceChecker) checkGroupByClause(groupClause GroupByClause, scope *refScope, path string) {
	switch clause := groupClause.(type) {
	case Serializer:
		c.checkExpressionWithOutputMode(clause, scope, path, outputsInputFirst)
	default:
		c.addOpaque(path, OpaqueCustom,
			fmt.Sprintf("GROUP BY clause type %T cannot be traversed by the checker", groupClause))
	}
}

func (c *referenceChecker) checkOrderByClause(orderClause OrderByClause, scope *refScope, path string) {
	switch clause := orderClause.(type) {
	case *orderByClauseImpl:
		c.checkExpressionWithOutputMode(clause.expression, scope, path, outputsOutputFirst)
	case Serializer:
		c.checkExpressionWithOutputMode(clause, scope, path, outputsOutputFirst)
	default:
		c.addOpaque(path, OpaqueCustom,
			fmt.Sprintf("ORDER BY clause type %T cannot be traversed by the checker", orderClause))
	}
}

func (c *referenceChecker) checkRowLock(lock RowLock, block *refScope, path string) {
	selectLock, ok := lock.(*selectLockImpl)
	if !ok {
		// Unknown lock implementation: nothing reference related can be inspected.
		return
	}

	for i, table := range selectLock.of {
		if is.Nil(table) {
			continue
		}

		name := table.Alias()
		if name == "" {
			name = table.TableName()
		}

		if _, ok := block.byName[name]; !ok {
			c.addError(indexPath(path, "of", i), ErrUnknownRelation, name, "",
				"FOR lock references a relation that is not introduced in the current query block")
		}
	}
}

// ---------------------------------------------------------------------
// WITH / CTE
// ---------------------------------------------------------------------

func (c *referenceChecker) checkWith(with *withImpl, outer *refScope, path string) {
	withScope := newScope(outer)

	for i, cte := range with.ctes {
		ctePath := indexPath(path, "cte", i)

		if cte == nil {
			continue
		}

		if _, duplicated := withScope.byName[cte.alias]; duplicated {
			c.addError(ctePath, ErrDuplicateRelation, cte.alias, "",
				"CTE name is introduced more than once in the same WITH clause")
		}

		// The CTE body can see outer scopes and the CTEs introduced before it. Reusing an outer
		// relation name for the CTE is legal shadowing: resolution simply picks the inner entry.
		cteOuter := newScope(withScope)

		if with.recursive {
			c.checkRecursiveCTEBody(cte, cteOuter, ctePath)
		} else {
			c.checkStatement(cte.Statement, cteOuter, ctePath+".body")
		}

		withScope.add(c.cteRelation(cte, ctePath), c.report, ctePath)
	}

	c.checkStatement(with.primaryStatement, newScope(withScope), path+".primary")
}

func (c *referenceChecker) checkRecursiveCTEBody(cte *CommonTableExpression, outer *refScope, ctePath string) {
	body := checkedUnwrap(cte.Statement)

	bodyStatement, ok := body.(*expressionStatementImpl)
	if !ok {
		// A single term is the non recursive term and must not reference the CTE itself.
		c.checkStatement(cte.Statement, outer, ctePath+".body")
		return
	}

	parsed := extractSelectClauses(bodyStatement.Clauses)
	if parsed.setOperator == nil {
		c.checkExpressionStatement(bodyStatement, outer, ctePath+".body")
		return
	}

	setOperator := parsed.setOperator

	for i, arm := range setOperator.Selects {
		armPath := indexPath(ctePath, "arm", i)

		armScope := outer
		if i > 0 {
			// Only the recursive terms (second arm and later) may self reference the CTE name.
			selfScope := newScope(outer)
			selfScope.add(&relation{
				name:    cte.alias,
				columns: c.explicitCTEColumns(cte),
				path:    ctePath,
				origin:  "recursive CTE self reference",
			}, c.report, ctePath)
			armScope = selfScope
		}

		c.checkStatement(arm, armScope, armPath)
	}

	if len(setOperator.OrderBy.List) > 0 {
		for i, orderClause := range setOperator.OrderBy.List {
			if is.Nil(orderClause) {
				continue
			}
			c.checkOrderByClause(orderClause, newScope(outer), indexPath(ctePath, "order_by", i))
		}
	}
}

func (c *referenceChecker) explicitCTEColumns(cte *CommonTableExpression) map[string]struct{} {
	if len(cte.Columns) == 0 {
		return nil
	}

	columns := make(map[string]struct{}, len(cte.Columns))
	for _, column := range cte.Columns {
		columns[column.Name()] = struct{}{}
	}

	return columns
}

func (c *referenceChecker) cteRelation(cte *CommonTableExpression, path string) *relation {
	columns := c.explicitCTEColumns(cte)
	if columns == nil && !is.Nil(cte.Statement) {
		if provider, ok := cte.Statement.(HasProjections); ok {
			columns = c.projectionColumns(provider.projections())
		}
	}

	return &relation{
		name:    cte.alias,
		columns: columns,
		path:    path,
		origin:  "CTE " + cte.alias,
	}
}

// ---------------------------------------------------------------------
// INSERT
// ---------------------------------------------------------------------

func (c *referenceChecker) checkInsert(stmt *statementImpl, outer *refScope, path string) {
	var insertClause *ClauseInsert
	var valuesQuery *ClauseValuesQuery
	var returning *ClauseReturning

	for _, clause := range stmt.Clauses {
		switch cl := clause.(type) {
		case *ClauseInsert:
			insertClause = cl
		case *ClauseValuesQuery:
			valuesQuery = cl
		case *ClauseReturning:
			returning = cl
		}
	}

	if insertClause == nil || is.Nil(insertClause.Table) {
		c.addError(path, ErrUnsupportedStatement, "", "", "INSERT statement is missing its target table")
		return
	}

	targetScope := newScope(outer)
	targetRel := c.tableRelation(insertClause.Table, path+".target")
	if targetRel != nil {
		targetScope.add(targetRel, c.report, path+".target")
	}

	if valuesQuery != nil {
		for rowIndex, row := range valuesQuery.Rows {
			for columnIndex, value := range row {
				if is.Nil(value) {
					continue
				}
				c.checkExpression(value, newScope(outer),
					indexPath(indexPath(path, "values", rowIndex), "column", columnIndex))
			}
		}

		if !is.Nil(valuesQuery.Query) {
			// An INSERT ... QUERY subquery cannot reference the insert target: it is checked with
			// only the outer scope as parent.
			c.checkStatement(valuesQuery.Query, newScope(outer), path+".query")
		}
	}

	// ON CONFLICT and any other dialect specific clause expose named, traversable children.
	for _, clause := range stmt.Clauses {
		if _, known := clause.(*ClauseInsert); known {
			continue
		}
		if _, known := clause.(*ClauseValuesQuery); known {
			continue
		}
		if _, known := clause.(*ClauseReturning); known {
			continue
		}

		c.checkDialectClause(clause, targetScope, path)
	}

	if returning != nil {
		for i, projection := range returning.ProjectionList {
			c.checkProjection(projection, targetScope, indexPath(path, "returning", i), nil)
		}
	}
}

// checkDialectClause handles clauses that are not part of the internal jet clause set, such as the
// PostgreSQL ON CONFLICT clause. Nodes named "action" are checked with the EXCLUDED pseudo relation
// in scope, the others with only the insert target.
func (c *referenceChecker) checkDialectClause(clause Clause, targetScope *refScope, path string) {
	provider, ok := clause.(CheckedNodeProvider)
	if !ok {
		c.addOpaque(path, OpaqueCustom, fmt.Sprintf("clause %T cannot be traversed by the checker", clause))
		return
	}

	for _, node := range provider.CheckedNodes() {
		if is.Nil(node.Node) {
			continue
		}

		nodePath := path + "." + node.Name
		nodeScope := targetScope

		if node.Name == "action" {
			nodeScope = c.conflictScope(targetScope, path)
		}

		if statement, isStatement := node.Node.(SerializerStatement); isStatement {
			c.checkStatement(statement, nodeScope, nodePath)
		} else {
			c.checkExpression(node.Node, nodeScope, nodePath)
		}
	}
}

func (c *referenceChecker) conflictScope(targetScope *refScope, path string) *refScope {
	conflictScope := newScope(targetScope.parent)
	conflictScope.rels = append(conflictScope.rels, targetScope.rels...)

	for name, rel := range targetScope.byName {
		conflictScope.byName[name] = rel
	}

	if len(targetScope.rels) > 0 && targetScope.rels[0].columns != nil {
		excludedColumns := make(map[string]struct{}, len(targetScope.rels[0].columns))
		for name := range targetScope.rels[0].columns {
			excludedColumns[name] = struct{}{}
		}

		conflictScope.add(&relation{
			name:    "excluded",
			columns: excludedColumns,
			path:    path,
			origin:  "ON CONFLICT EXCLUDED pseudo relation",
		}, c.report, path+".excluded")
	}

	return conflictScope
}

// ---------------------------------------------------------------------
// FROM / JOIN / derived tables / LATERAL
// ---------------------------------------------------------------------

func (c *referenceChecker) checkFromItems(tables []Serializer, block *refScope, outer *refScope, path string) {
	for i, item := range tables {
		itemPath := indexPath(path, "from", i)
		relations := c.introduce(item, block, outer, itemPath)

		for _, rel := range relations {
			block.add(rel, c.report, itemPath)
		}
	}
}

// introduce resolves one FROM item into the relations it contributes. left holds the relations
// already visible on its left in the same query block, outer is the legal outer query scope.
func (c *referenceChecker) introduce(node Serializer, left *refScope, outer *refScope, path string) []*relation {
	if is.Nil(node) {
		return nil
	}

	node = checkedUnwrap(node)
	if is.Nil(node) {
		return nil
	}

	switch item := node.(type) {
	case *tableImpl:
		return []*relation{c.tableRelationImpl(item, path)}

	case *joinTableImpl:
		return c.introduceJoin(item, left, outer, path)

	case lateralImpl:
		return []*relation{c.introduceDerived(item.selectTableImpl, true, left, outer, path)}

	case selectTableImpl:
		return []*relation{c.introduceDerived(item, false, left, outer, path)}

	case CommonTableExpression:
		// A CTE is only visible from later CTE definitions and from the WITH primary statement.
		// Referencing it before it is introduced (including a non recursive self reference) fails
		// exactly like referencing an unknown table alias.
		if !c.cteReferenceIsVisible(item.alias, left, outer, path) {
			return nil
		}

		return []*relation{c.cteRelation(&item, path)}

	case Values:
		return []*relation{
			{
				name:    "",
				columns: c.projectionColumns(item.projections()),
				path:    path,
				origin:  "VALUES table constructor",
			},
		}

	default:
		c.addOpaque(path, OpaqueCustom,
			fmt.Sprintf("FROM item type %T cannot be traversed by the checker", node))
		return nil
	}
}

func (c *referenceChecker) introduceJoin(join *joinTableImpl, left *refScope, outer *refScope, path string) []*relation {
	lhsRels := c.introduce(join.lhs, left, outer, path+".lhs")

	joinLeft := c.combineScopes(left, lhsRels)
	rhsRels := c.introduce(join.rhs, joinLeft, outer, path+".rhs")

	if !is.Nil(join.onCondition) {
		onScope := newScope(outer)

		for _, rel := range append(lhsRels, rhsRels...) {
			onScope.add(rel, c.report, path+".on")
		}

		// Same block relations outside of this join subtree exist but are not part of the two
		// sides of the current join.
		for _, rel := range left.rels {
			if _, covered := onScope.byName[rel.name]; !covered {
				onScope.hidden[rel.name] = boundaryJoinOn
			}
		}

		c.checkExpression(join.onCondition, onScope, path+".on")
	}

	return append(lhsRels, rhsRels...)
}

func (c *referenceChecker) combineScopes(left *refScope, extra []*relation) *refScope {
	combined := newScope(left.parent)
	combined.rels = append(combined.rels, left.rels...)

	for name, rel := range left.byName {
		combined.byName[name] = rel
	}

	for name, boundary := range left.hidden {
		combined.hidden[name] = boundary
	}

	for _, rel := range extra {
		combined.add(rel, c.report, rel.path)
	}

	return combined
}

// cteReferenceIsVisible reports whether a CTE name used as a FROM relation is introduced in the
// visible scope chain. It records an unknown relation error when the CTE is referenced too early.
func (c *referenceChecker) cteReferenceIsVisible(name string, left, outer *refScope, path string) bool {
	if _, visible := findRelationInChain(left, name); visible != nil {
		return true
	}

	if _, outerVisible := findRelationInChain(outer, name); outerVisible != nil {
		return true
	}

	c.addError(path, ErrUnknownRelation, name, "",
		"CTE name is not introduced at this location: a CTE body can only reference CTEs defined earlier in the same WITH clause")

	return false
}

// checkDerivedBody checks the statement embedded in a derived table. Besides regular statements the
// embedded object can be a CTE alias (CTE.ALIAS), a VALUES constructor or another select table.
func (c *referenceChecker) checkDerivedBody(node Serializer, scope *refScope, path string) {
	node = checkedUnwrap(node)
	if is.Nil(node) {
		return
	}

	switch body := node.(type) {
	case CommonTableExpression:
		c.cteReferenceIsVisible(body.alias, scope, scope.parent, path)
	case selectTableImpl:
		c.checkDerivedBody(body.Statement, scope, path+".body")
	case Values:
		// Constant table constructor, no references to check.
	default:
		c.checkStatement(node, scope, path)
	}
}

func (c *referenceChecker) introduceDerived(
	table selectTableImpl,
	lateral bool,
	left *refScope,
	outer *refScope,
	path string,
) *relation {
	if lateral {
		// LATERAL sees only the relations on its left plus the legal outer scope.
		lateralScope := newScope(outer)
		for _, rel := range left.rels {
			lateralScope.add(rel, c.report, rel.path)
		}
		for name, boundary := range left.hidden {
			lateralScope.hidden[name] = boundary
		}
		for _, rel := range blockRelationsOutside(left, lateralScope) {
			lateralScope.hidden[rel.name] = boundaryLateral
		}

		c.checkDerivedBody(table.Statement, lateralScope, path+".body")
	} else {
		// A plain derived table cannot reference same block siblings: only outer scopes are legal.
		derivedOuter := newScope(outer)
		for _, rel := range left.rels {
			derivedOuter.hidden[rel.name] = boundaryCorrelation
		}
		for name, boundary := range left.hidden {
			if _, exists := derivedOuter.hidden[name]; !exists {
				derivedOuter.hidden[name] = boundary
			}
		}

		c.checkDerivedBody(table.Statement, derivedOuter, path+".body")
	}

	columns := c.derivedTableColumns(table)

	return &relation{
		name:    table.alias,
		columns: columns,
		path:    path,
		origin:  derivedOrigin(table, lateral),
	}
}

func derivedOrigin(table selectTableImpl, lateral bool) string {
	if lateral {
		return "LATERAL derived table " + table.alias
	}

	return "derived table " + table.alias
}

// blockRelationsOutside returns left scope relations that did not make it into the lateral scope
// (i.e. relations positioned to the right of the LATERAL entry).
func blockRelationsOutside(left *refScope, lateralScope *refScope) []*relation {
	var outside []*relation

	for _, rel := range left.rels {
		if _, visible := lateralScope.byName[rel.name]; !visible {
			outside = append(outside, rel)
		}
	}

	return outside
}

func (c *referenceChecker) derivedTableColumns(table selectTableImpl) map[string]struct{} {
	if len(table.columnAliases) > 0 {
		columns := make(map[string]struct{}, len(table.columnAliases))
		for _, column := range table.columnAliases {
			columns[column.Name()] = struct{}{}
		}
		return columns
	}

	if is.Nil(table.Statement) {
		return nil
	}

	provider, ok := table.Statement.(HasProjections)
	if !ok {
		return nil
	}

	return c.projectionColumns(provider.projections())
}

func (c *referenceChecker) tableRelation(table Serializer, path string) *relation {
	table = checkedUnwrap(table)

	if impl, ok := table.(*tableImpl); ok {
		return c.tableRelationImpl(impl, path)
	}

	c.addOpaque(path, OpaqueCustom,
		fmt.Sprintf("table type %T cannot be traversed by the checker", table))
	return nil
}

func (c *referenceChecker) tableRelationImpl(impl *tableImpl, path string) *relation {
	name := impl.alias
	if name == "" {
		name = impl.name
	}

	columns := make(map[string]struct{}, len(impl.columnList))
	for _, column := range impl.columnList {
		columns[column.Name()] = struct{}{}
	}

	origin := impl.name
	if impl.schemaName != "" {
		origin = impl.schemaName + "." + impl.name
	}

	return &relation{
		name:    name,
		columns: columns,
		path:    path,
		origin:  "physical table " + origin,
	}
}

// projectionColumns derives the statically known output column names of a relation from its
// projections. A nil result means at least one output name is not statically known.
func (c *referenceChecker) projectionColumns(projections ProjectionList) map[string]struct{} {
	if len(projections) == 0 {
		return nil
	}

	columns := map[string]struct{}{}

	for _, projection := range projections {
		switch p := projection.(type) {
		case ProjectionList:
			nested := c.projectionColumns(p)
			if nested == nil {
				return nil
			}
			for name := range nested {
				columns[name] = struct{}{}
			}
		case ColumnList:
			for _, column := range p {
				columns[column.defaultAlias()] = struct{}{}
			}
		case ColumnExpression:
			columns[p.defaultAlias()] = struct{}{}
		case *alias:
			columns[p.alias] = struct{}{}
		default:
			return nil
		}
	}

	return columns
}

func (c *referenceChecker) collectOutputs(projections ProjectionList, outputs map[string]struct{}) {
	if outputs == nil {
		return
	}

	for _, projection := range projections {
		switch p := projection.(type) {
		case ProjectionList:
			c.collectOutputs(p, outputs)
		case ColumnList:
			for _, column := range p {
				outputs[column.defaultAlias()] = struct{}{}
			}
		case ColumnExpression:
			outputs[p.defaultAlias()] = struct{}{}
		case *alias:
			outputs[p.alias] = struct{}{}
		}
	}
}

// ---------------------------------------------------------------------
// Projection and expression walking
// ---------------------------------------------------------------------

func (c *referenceChecker) checkProjection(projection Projection, scope *refScope, path string, outputs map[string]struct{}) {
	if is.Nil(projection) {
		return
	}

	switch p := projection.(type) {
	case ProjectionList:
		for i, nested := range p {
			c.checkProjection(nested, scope, indexPath(path, "projections", i), outputs)
		}
	case ColumnList:
		for i, column := range p {
			c.checkExpression(column, scope, indexPath(path, "columns", i))
		}
	case *alias:
		if outputs != nil {
			outputs[p.alias] = struct{}{}
		}
		c.checkExpression(p.expression, scope, path+".expression")
	case Serializer:
		c.checkExpression(p, scope, path)
	default:
		c.addOpaque(path, OpaqueCustom,
			fmt.Sprintf("projection type %T cannot be traversed by the checker", projection))
	}
}

// wrappedExpression is implemented by every typed expression wrapper around a base expression.
type wrappedExpression interface {
	wrappedExpression() Expression
}

func (e expressionWrapper) wrappedExpression() Expression { return e.expression }

// windowCarrier is implemented by every window expression container.
type windowCarrier interface {
	windowParts() (expression Expression, window Window)
}

func (w *commonWindowImpl) windowParts() (Expression, Window) { return w.expression, w.window }

func (c *referenceChecker) checkExpression(node Serializer, scope *refScope, path string) {
	c.checkExpressionWithOutputMode(node, scope, path, outputsHidden)
}

func (c *referenceChecker) checkExpressionWithOutputMode(
	node Serializer,
	scope *refScope,
	path string,
	mode outputResolution,
) {
	if is.Nil(node) {
		return
	}

	node = checkedUnwrap(node)
	if is.Nil(node) {
		return
	}

	// A statement used in expression position is a scalar/dependent subquery: outer scope
	// relations are visible (correlation), same block siblings are not.
	if statement, ok := node.(SerializerStatement); ok {
		c.checkStatement(statement, scope, path+".subquery")
		return
	}

	switch expr := node.(type) {
	case ColumnList:
		for i, column := range expr {
			c.checkExpressionWithOutputMode(column, scope, indexPath(path, "columns", i), mode)
		}

	case columnRefProvider:
		c.resolveColumn(expr.columnRefInfo(), scope, path, mode)

	case *expression:
		c.checkExpressionWithOutputMode(expr.Serializer, scope, path, mode)

	case wrappedExpression:
		c.checkExpressionWithOutputMode(expr.wrappedExpression(), scope, path, mode)

	case windowCarrier:
		windowExpression, window := expr.windowParts()
		c.checkExpressionWithOutputMode(windowExpression, scope, path+".over", mode)
		if !is.Nil(window) {
			c.checkExpressionWithOutputMode(window, scope, path+".window", mode)
		}

	case *customSerializer:
		for i, part := range expr.parts {
			c.checkExpressionWithOutputMode(part, scope, indexPath(path, "parts", i), mode)
		}

	case *binaryOperatorSerializer:
		c.checkExpressionWithOutputMode(expr.lhs, scope, path+".lhs", mode)
		c.checkExpressionWithOutputMode(expr.rhs, scope, path+".rhs", mode)
		if !is.Nil(expr.additionalParam) {
			c.checkExpressionWithOutputMode(expr.additionalParam, scope, path+".param", mode)
		}

	case *serializersWithOperator:
		for i, serializer := range expr.serializers {
			c.checkExpressionWithOutputMode(serializer, scope, indexPath(path, "operands", i), mode)
		}

	case *betweenOperatorSerializer:
		c.checkExpressionWithOutputMode(expr.expression, scope, path+".expression", mode)
		c.checkExpressionWithOutputMode(expr.min, scope, path+".min", mode)
		c.checkExpressionWithOutputMode(expr.max, scope, path+".max", mode)

	case *funcSerializer:
		c.checkSerializers(expr.parameters, scope, path+".args", mode)

	case parametersSerializer:
		c.checkSerializers(expr, scope, path, mode)

	case *caseOperatorImpl:
		if !is.Nil(expr.expression) {
			c.checkExpressionWithOutputMode(expr.expression, scope, path+".expression", mode)
		}
		for i := range expr.when {
			c.checkExpressionWithOutputMode(expr.when[i], scope, indexPath(path, "when", i), mode)
			c.checkExpressionWithOutputMode(expr.then[i], scope, indexPath(path, "then", i), mode)
		}
		if !is.Nil(expr.els) {
			c.checkExpressionWithOutputMode(expr.els, scope, path+".else", mode)
		}

	case *regexpLikeSerializer:
		c.checkExpressionWithOutputMode(expr.str, scope, path+".str", mode)
		c.checkExpressionWithOutputMode(expr.pattern, scope, path+".pattern", mode)

	case *windowImpl:
		c.checkWindow(expr, scope, path, mode)

	case windowName:
		c.checkWindow(&expr.windowImpl, scope, path, mode)

	case *frameExtentImpl:
		c.checkExpressionWithOutputMode(expr.offset, scope, path+".offset", mode)

	case frameExtentKeyword:
		// keyword only, no references

	case *orderSetAggregateFuncSerializer:
		if !is.Nil(expr.fraction) {
			c.checkExpressionWithOutputMode(expr.fraction, scope, path+".fraction", mode)
		}
		if !is.Nil(expr.orderBy) {
			c.checkExpressionWithOutputMode(expr.orderBy, scope, path+".within_group", mode)
		}

	case ListSerializer:
		for i, serializer := range expr.Serializers {
			c.checkExpressionWithOutputMode(serializer, scope, indexPath(path, "items", i), mode)
		}

	case *serializerImpl:
		for i, clause := range expr.Clauses {
			c.checkClause(clause, scope, indexPath(path, "clause", i), mode)
		}

	case *columnAssigmentImpl:
		c.checkExpressionWithOutputMode(expr.column, scope, path+".column", mode)
		c.checkExpressionWithOutputMode(expr.toAssign, scope, path+".value", mode)

	case columnAssigmentImpl:
		c.checkExpressionWithOutputMode(expr.column, scope, path+".column", mode)
		c.checkExpressionWithOutputMode(expr.toAssign, scope, path+".value", mode)

	case columnListAssigment:
		for i, assignment := range expr {
			c.checkExpressionWithOutputMode(assignment, scope, indexPath(path, "assignments", i), mode)
		}

	case JsonObjProjectionList:
		for i, projection := range expr {
			c.checkProjection(projection, scope, indexPath(path, "entries", i), nil)
		}

	case *rowExpressionWrapper:
		c.checkExpressionWithOutputMode(expr.Expression, scope, path, mode)

	case Values:
		for i, row := range expr {
			c.checkExpressionWithOutputMode(row, scope, indexPath(path, "rows", i), mode)
		}

	case *literalSerializer, Keyword, Token, enumValueSerializer:
		// reference free leaves

	case *rawSerializer:
		c.addOpaque(path, OpaqueRaw, "jet Raw expression is an opaque SQL string")

	default:
		c.addOpaque(path, OpaqueCustom,
			fmt.Sprintf("serializer type %T cannot be traversed by the checker", node))
	}
}

func (c *referenceChecker) checkSerializers(serializers []Expression, scope *refScope, path string, mode outputResolution) {
	for i, serializer := range serializers {
		c.checkExpressionWithOutputMode(serializer, scope, indexPath(path, "items", i), mode)
	}
}

func (c *referenceChecker) checkWindow(window *windowImpl, scope *refScope, path string, mode outputResolution) {
	for i, expression := range window.partitionBy {
		c.checkExpressionWithOutputMode(expression, scope, indexPath(path, "partition_by", i), mode)
	}

	for i, orderClause := range window.orderBy.List {
		if is.Nil(orderClause) {
			continue
		}
		c.checkOrderByClause(orderClause, scope, indexPath(path, "order_by", i))
	}

	if !is.Nil(window.start) {
		c.checkExpressionWithOutputMode(window.start, scope, path+".frame_start", mode)
	}
	if !is.Nil(window.end) {
		c.checkExpressionWithOutputMode(window.end, scope, path+".frame_end", mode)
	}
}

func (c *referenceChecker) checkClause(clause Clause, scope *refScope, path string, mode outputResolution) {
	switch cl := clause.(type) {
	case *ClauseWhere:
		if !is.Nil(cl.Condition) {
			c.checkExpressionWithOutputMode(cl.Condition, scope, path+".condition", outputsHidden)
		}
	case *KeywordClause, KeywordClause:
		// keyword only, no references
	case *SetClauseNew:
		for i, assignment := range *cl {
			c.checkExpressionWithOutputMode(assignment, scope, indexPath(path, "assignments", i), mode)
		}
	case SetClauseNew:
		for i, assignment := range cl {
			c.checkExpressionWithOutputMode(assignment, scope, indexPath(path, "assignments", i), mode)
		}
	case CheckedNodeProvider:
		for _, node := range cl.CheckedNodes() {
			if is.Nil(node.Node) {
				continue
			}
			c.checkExpressionWithOutputMode(node.Node, scope, path+"."+node.Name, mode)
		}
	default:
		c.addOpaque(path, OpaqueCustom,
			fmt.Sprintf("clause type %T cannot be traversed by the checker", clause))
	}
}

// ---------------------------------------------------------------------
// Column reference resolution
// ---------------------------------------------------------------------

func (c *referenceChecker) resolveColumn(ref columnRefInfo, scope *refScope, path string, mode outputResolution) {
	if ref.qualifier != "" {
		c.resolveQualified(ref, scope, path, mode)
		return
	}

	c.resolveUnqualified(ref, scope, path, mode)
}

func (c *referenceChecker) resolveQualified(ref columnRefInfo, scope *refScope, path string, mode outputResolution) {
	_, rel := scope.findRelation(ref.qualifier)
	if rel != nil {
		c.checkColumnMembership(rel, ref.column, ref.qualifier, path)
		return
	}

	// ORDER BY of a set operation (and any ORDER BY whose FROM relations are out of scope)
	// references output columns by their serialized output name. A qualified reference
	// "qualifier"."column" matches the output alias "qualifier.column".
	if mode == outputsOutputFirst {
		if c.outputsContain(scope, ref.qualifier+"."+ref.column) {
			return
		}
	}

	if boundary := scope.findHidden(ref.qualifier); boundary != "" {
		c.boundaryError(boundary, ref.qualifier, path)
		return
	}

	available := scope.availableRelationNames()
	sort.Strings(available)

	c.addError(path, ErrUnknownRelation, ref.qualifier, ref.column,
		fmt.Sprintf("relation %q is not introduced at this location; visible relations: %v",
			ref.qualifier, available))
}

func (c *referenceChecker) boundaryError(boundary, relationName, path string) {
	switch boundary {
	case boundaryCorrelation:
		c.addError(path, ErrIllegalCorrelation, relationName, "",
			"non LATERAL derived table cannot reference another FROM item of the same query block; use LATERAL to correlate with preceding relations")
	case boundaryLateral:
		c.addError(path, ErrLateralBoundary, relationName, "",
			"LATERAL derived table can only reference relations introduced to its left")
	default:
		c.addError(path, ErrUnknownRelation, relationName, "",
			"JOIN ON clause can only reference the two sides of its join and the legal outer query scopes")
	}
}

func (c *referenceChecker) checkColumnMembership(rel *relation, column, qualifier, path string) {
	if rel.columns == nil {
		// The output column set of this relation is not statically known, so membership is left
		// unproven rather than rejected. This is not a database column type check.
		return
	}

	if _, ok := rel.columns[column]; ok {
		return
	}

	c.addError(path, ErrUnknownColumn, qualifier, column,
		fmt.Sprintf("column %q is not among the statically known output columns of %s (relation %q)",
			column, rel.origin, rel.name))
}

func (c *referenceChecker) outputsContain(scope *refScope, name string) bool {
	for current := scope; current != nil; current = current.parent {
		if _, ok := current.outputs[name]; ok {
			return true
		}
	}

	return false
}

func (c *referenceChecker) resolveUnqualified(ref columnRefInfo, scope *refScope, path string, mode outputResolution) {
	if mode == outputsOutputFirst && c.outputsContain(scope, ref.column) {
		return
	}

	var ambiguous bool

	for current := scope; current != nil; current = current.parent {
		var matches []string

		hasUnknownColumnSet := false

		for _, rel := range current.rels {
			if rel.columns == nil {
				hasUnknownColumnSet = true
				continue
			}

			if _, ok := rel.columns[ref.column]; ok {
				matches = append(matches, rel.name)
			}
		}

		if len(matches) > 1 {
			sort.Strings(matches)
			c.addError(path, ErrAmbiguousColumn, "", ref.column,
				fmt.Sprintf("unqualified column %q matches more than one relation: %v",
					ref.column, matches))
			ambiguous = true

			return
		}

		if len(matches) == 1 {
			return
		}

		if hasUnknownColumnSet {
			// A relation in this scope might expose the column but its shape is not known, so the
			// reference can neither be confirmed nor rejected.
			return
		}
	}

	if ambiguous {
		return
	}

	if mode == outputsInputFirst && c.outputsContain(scope, ref.column) {
		return
	}

	c.addError(path, ErrUnknownColumn, "", ref.column,
		fmt.Sprintf("unqualified column %q cannot be resolved to any visible relation", ref.column))
}
