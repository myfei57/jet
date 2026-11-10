package postgres

import (
	"errors"
	"fmt"

	"github.com/go-jet/jet/v2/internal/jet"
)

// SeekDirection selects whether a seek page returns the rows strictly after or strictly
// before the supplied cursor.
type SeekDirection int

const (
	// SeekAfter returns the rows positioned strictly after the cursor row (next page).
	SeekAfter SeekDirection = iota
	// SeekBefore returns the rows positioned strictly before the cursor row (previous page).
	SeekBefore
)

// SeekNullsOrder is the explicit NULL position of one seek sort dimension.
type SeekNullsOrder int

const (
	// SeekNullsFirst emits NULLS FIRST.
	SeekNullsFirst SeekNullsOrder = iota
	// SeekNullsLast emits NULLS LAST.
	SeekNullsLast
)

// SeekResultOrder documents the order in which the rows of a seek page are returned.
type SeekResultOrder int

const (
	// SeekCanonicalOrder means the page rows are ordered exactly as declared by the
	// SeekSort list followed by the unique key. SeekAfter pages always use this order.
	SeekCanonicalOrder SeekResultOrder = iota
	// SeekReverseOrder means the page rows are ordered in the reverse of the order declared
	// by the SeekSort list followed by the unique key. SeekBefore pages always use this order:
	// the first row of the page is the immediate predecessor of the cursor row, i.e. the page
	// contains the nearest preceding rows, never the farthest ones. Reverse the returned page
	// in application code when canonical order is required.
	SeekReverseOrder
)

// SeekSort is one non-volatile, deterministic, FROM-visible sort dimension of a seek page.
//
// The same Expression is serialized unchanged in both the generated seek predicate and the
// ORDER BY clause; it is never wrapped, replaced by an alias or rewritten with COALESCE-style
// sentinels. Supported expressions are table/sub-query columns and other library-built
// expressions (functions, operators, casts, CASE, ...). Raw expressions are rejected by
// NewSeekPage unless SeekPageRequest.AllowRawExpressions is set.
//
// Aliased projections (Expression.AS(...)) are Projection values rather than Expression
// values, so they cannot be passed here: an output alias is not a WHERE-visible bare column.
type SeekSort struct {
	// Expression is the repeatable, order-comparable expression of this dimension.
	Expression Expression
	// Ascending selects ASC when true and DESC when false.
	Ascending bool
	// Nulls is always required and is emitted as an explicit NULLS FIRST / NULLS LAST.
	Nulls SeekNullsOrder
}

// SeekUniqueKey is the mandatory final tie-breaking dimension of a seek page.
//
// The unique key expression must reference a NOT NULL, unique column (or an expression that is
// guaranteed to be unique and non-null over the result). It makes the total ordering total even
// when every other sort dimension has equal values.
type SeekUniqueKey struct {
	// Expression is the unique key expression, normally a table column.
	Expression Column
	// Ascending selects the unique key direction; ASC is the common choice.
	Ascending bool
	// Value is the unique key value of the cursor row. It is ignored on the first page
	// (nil SeekPageRequest.Cursor) and must be provided on every subsequent page.
	Value interface{}
}

// SeekPageRequest is the validated input of NewSeekPage.
type SeekPageRequest struct {
	// Direction selects a next (SeekAfter) or previous (SeekBefore) page.
	Direction SeekDirection
	// Sorts is the non-empty ordered list of sort dimensions.
	Sorts []SeekSort
	// UniqueKey is the mandatory final tie-breaking dimension.
	UniqueKey SeekUniqueKey
	// Cursor holds one typed cursor value per entry of Sorts, in the same order. A nil element
	// represents an SQL NULL cursor value and is sent as a parameterized NULL (never as a
	// COALESCE sentinel). A nil slice means the first page, for which no seek predicate is
	// generated and UniqueKey.Value is ignored.
	Cursor []interface{}
	// Limit is the maximum page size and must be strictly positive. It is emitted as a
	// parameterized LIMIT; OFFSET is never used.
	Limit int64
	// AllowRawExpressions opts into Raw(...) sort expressions. By setting it the caller
	// explicitly assumes the capability contract: the raw text must be non-volatile,
	// repeatable across the WHERE and ORDER BY evaluations, reference only FROM-visible
	// columns and produce values comparable with the supplied cursor values.
	AllowRawExpressions bool
}

// SeekPage contains the generated, ready-to-apply seek clauses.
type SeekPage struct {
	// Predicate is the strict lexicographic seek predicate. It is nil for the first page.
	// The cursor row itself never satisfies the predicate.
	Predicate BoolExpression
	// OrderBy carries an explicit ASC/DESC and NULLS FIRST/LAST for every sort dimension
	// followed by the unique key.
	OrderBy []OrderByClause
	// Limit is the validated, parameterized page size.
	Limit int64
	// Direction is the requested page direction.
	Direction SeekDirection
	// ResultOrder is the order in which the page rows are returned (see SeekResultOrder).
	ResultOrder SeekResultOrder
}

// seekDimension is the internally effective (direction/NULLS already resolved) sort dimension.
type seekDimension struct {
	expr       jet.Expression
	ascending  bool
	nullsFirst bool
	cursor     interface{}
}

// NewSeekPage validates the request and builds a SeekPage containing the strict after/before
// predicate, the matching ORDER BY clauses and the LIMIT.
//
// The predicate is expanded lexicographically: every OR branch keeps all preceding dimensions
// NULL-safe equal (IS NOT DISTINCT FROM) and applies a strict ordering comparison on the current
// dimension that simultaneously accounts for its ASC/DESC direction and its NULLS FIRST/LAST
// position. NULL cursor values therefore cross NULL groups correctly, and the cursor row is
// never included again.
//
// For SeekBefore, every direction and NULL position is reversed before building the predicate,
// so the query fetches the nearest preceding page; see SeekReverseOrder for the result protocol.
//
// NewSeekPage does not touch any SELECT statement and does not rewrite the supplied expressions.
// Cursor and LIMIT values are emitted as ordinary parameters, so their placeholders continue the
// numbering of the SQLBuilder they are serialized into and never collide with sub-query
// parameters.
func NewSeekPage(request SeekPageRequest) (*SeekPage, error) {
	if err := validateSeekRequest(request); err != nil {
		return nil, err
	}

	dimensions := make([]seekDimension, 0, len(request.Sorts)+1)

	for i, sort := range request.Sorts {
		dimensions = append(dimensions, seekDimension{
			expr:       sort.Expression,
			ascending:  sort.Ascending,
			nullsFirst: sort.Nulls == SeekNullsFirst,
			cursor:     seekCursorValue(request.Cursor, i),
		})
	}

	// The unique key is the final, total-ordering dimension. It is declared NOT NULL by contract,
	// so NULLS LAST is the natural placement; SeekBefore flips it together with every other
	// dimension.
	dimensions = append(dimensions, seekDimension{
		expr:       request.UniqueKey.Expression,
		ascending:  request.UniqueKey.Ascending,
		nullsFirst: false,
		cursor:     request.UniqueKey.Value,
	})

	if request.Direction == SeekBefore {
		for i := range dimensions {
			dimensions[i].ascending = !dimensions[i].ascending
			dimensions[i].nullsFirst = !dimensions[i].nullsFirst
		}
	}

	page := &SeekPage{
		Limit:     request.Limit,
		Direction: request.Direction,
		OrderBy:   buildSeekOrderBy(dimensions),
	}

	if request.Direction == SeekBefore {
		page.ResultOrder = SeekReverseOrder
	} else {
		page.ResultOrder = SeekCanonicalOrder
	}

	// No cursor means the first page: only ORDER BY and LIMIT apply.
	if request.Cursor != nil {
		page.Predicate = buildSeekPredicate(dimensions)
	}

	return page, nil
}

func validateSeekRequest(request SeekPageRequest) error {
	if request.Direction != SeekAfter && request.Direction != SeekBefore {
		return fmt.Errorf("jet: seek page invalid direction %d", request.Direction)
	}

	if len(request.Sorts) == 0 {
		return errors.New("jet: seek page requires at least one sort expression")
	}

	if request.UniqueKey.Expression == nil {
		return errors.New("jet: seek page requires a non-nil unique key expression")
	}

	if request.Limit <= 0 {
		return fmt.Errorf("jet: seek page limit must be strictly positive, got %d", request.Limit)
	}

	if request.Cursor != nil && len(request.Cursor) != len(request.Sorts) {
		return fmt.Errorf(
			"jet: seek page cursor dimension mismatch: %d cursor value(s) for %d sort expression(s)",
			len(request.Cursor), len(request.Sorts))
	}

	for i, sort := range request.Sorts {
		if sort.Expression == nil {
			return fmt.Errorf("jet: seek page sort expression at index %d is nil", i)
		}

		if !request.AllowRawExpressions && jet.IsRawExpression(sort.Expression) {
			return fmt.Errorf(
				"jet: seek page sort expression at index %d is a Raw expression: set "+
					"SeekPageRequest.AllowRawExpressions to accept the raw capability contract", i)
		}
	}

	return nil
}

func seekCursorValue(cursor []interface{}, index int) interface{} {
	if cursor == nil {
		return nil
	}

	return cursor[index]
}

func buildSeekOrderBy(dimensions []seekDimension) []OrderByClause {
	orderBy := make([]OrderByClause, len(dimensions))

	for i, dimension := range dimensions {
		var clause jet.OrderByClause
		if dimension.ascending {
			clause = dimension.expr.ASC()
		} else {
			clause = dimension.expr.DESC()
		}

		if dimension.nullsFirst {
			clause = clause.NULLS_FIRST()
		} else {
			clause = clause.NULLS_LAST()
		}

		orderBy[i] = clause
	}

	return orderBy
}

// buildSeekPredicate expands the lexicographic "strictly after" predicate over the effective
// dimensions. The same routine serves SeekBefore because its dimensions arrive already reversed.
func buildSeekPredicate(dimensions []seekDimension) BoolExpression {
	var branches []BoolExpression
	var prefixEqual []BoolExpression

	for i, dimension := range dimensions {
		strict := buildSeekStrictComparison(dimension)

		if strict != nil {
			branchParts := make([]BoolExpression, 0, len(prefixEqual)+1)
			branchParts = append(branchParts, prefixEqual...)
			branchParts = append(branchParts, strict)
			branches = append(branches, jet.AND(branchParts...))
		}

		// The equality prefix of the last dimension is never part of any later branch.
		if i < len(dimensions)-1 {
			prefixEqual = append(prefixEqual,
				jet.IsNotDistinctFrom(dimension.expr, jet.Literal(dimension.cursor)))
		}
	}

	if len(branches) == 0 {
		// Every dimension is statically unable to move forward (e.g. all-NULL cursor with
		// NULLS LAST everywhere): the correct result is an empty page.
		return jet.Bool(false)
	}

	return jet.OR(branches...)
}

// buildSeekStrictComparison builds the "row value is strictly after the cursor value" relation
// for one dimension, taking both the effective direction and the effective NULL position into
// account. It returns nil when the relation is statically impossible, in which case the
// corresponding OR branch is omitted.
func buildSeekStrictComparison(dimension seekDimension) BoolExpression {
	if dimension.cursor == nil {
		// NULL is the smallest value under NULLS FIRST: every non-NULL row is after it.
		// NULL is the largest value under NULLS LAST: nothing is after it.
		if dimension.nullsFirst {
			return dimension.expr.IS_NOT_NULL()
		}

		return nil
	}

	cursorValue := jet.Literal(dimension.cursor)

	var valueComparison BoolExpression
	if dimension.ascending {
		valueComparison = jet.Gt(dimension.expr, cursorValue)
	} else {
		valueComparison = jet.Lt(dimension.expr, cursorValue)
	}

	if dimension.nullsFirst {
		// NULL sorts before every non-NULL value, so a strictly greater row must be non-NULL
		// and pass the directional value comparison.
		return jet.AND(dimension.expr.IS_NOT_NULL(), valueComparison)
	}

	// NULL sorts after every non-NULL value, so a NULL row is always strictly after a
	// non-NULL cursor; otherwise the directional value comparison decides.
	return jet.OR(dimension.expr.IS_NULL(), valueComparison)
}

// ApplyTo returns a new SelectStatement carrying the seek WHERE predicate, the matching ORDER BY
// and LIMIT. The input statement is never modified: it is value-copied together with its clauses,
// and its original SELECT projections and expressions are serialized unchanged. An existing WHERE
// condition is combined with the seek predicate using AND.
func (page *SeekPage) ApplyTo(selectStatement SelectStatement) SelectStatement {
	if page == nil {
		panic("jet: seek page is nil")
	}

	statement, ok := selectStatement.(*selectStatementImpl)
	if !ok {
		panic("jet: seek page can only be applied to a postgres.SelectStatement")
	}

	clone := *statement
	clone.ExpressionStatement = jet.NewExpressionStatementImpl(Dialect, jet.SelectStatementType, &clone,
		&clone.Select,
		&clone.From,
		&clone.Where,
		&clone.GroupBy,
		&clone.Having,
		&clone.Window,
		&clone.OrderBy,
		&clone.Limit,
		&clone.Offset,
		&clone.Fetch,
		&clone.For)
	clone.setOperatorsImpl.stmtRoot = &clone

	if page.Predicate != nil {
		if clone.Where.Condition != nil {
			clone.WHERE(AND(clone.Where.Condition, page.Predicate))
		} else {
			clone.WHERE(page.Predicate)
		}
	}

	clone.ORDER_BY(page.OrderBy...)
	clone.LIMIT(page.Limit)

	return &clone
}
