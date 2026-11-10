package jet

import (
	"reflect"
	"strings"

	"github.com/go-jet/jet/v2/internal/utils/is"
)

// SerializerInspection summarizes properties of a serializer tree that can be discovered
// statically, without serializing or executing it.
type SerializerInspection struct {
	// HasOpaque is true when the tree contains a serializer whose SQL text cannot be inspected.
	// This covers raw SQL fragments (Raw, RawBool, ...) and user provided Serializer implementations,
	// which may hide volatile or side-effecting calls (for example random() or nextval()).
	HasOpaque bool

	// HasStatement is true when the tree embeds a statement, sub-query, table or row lock serializer,
	// whose result may depend on the enclosing single-statement execution.
	HasStatement bool

	// VolatileFunctions lists upper-case names of the detected functions marked as volatile.
	VolatileFunctions []string
}

// RequiresExplicitOptIn reports whether the inspected tree contains a construct that must be
// explicitly accepted before an INSERT can be split into multiple independent statements.
func (i SerializerInspection) RequiresExplicitOptIn() bool {
	return i.HasOpaque || i.HasStatement || len(i.VolatileFunctions) > 0
}

// InspectSerializer walks a serializer tree without serializing or executing it.
// volatileFuncNames is a set of upper-case function names that must be reported as volatile.
func InspectSerializer(root Serializer, volatileFuncNames map[string]bool) SerializerInspection {
	inspection := &SerializerInspection{}
	visited := map[uintptr]bool{}
	inspectSerializer(root, volatileFuncNames, visited, inspection)
	return *inspection
}

// InspectProjection walks a projection tree without serializing or executing it. Projections do
// not all implement Serializer (aliased expressions and projection lists are projection-only
// types), so projection contexts such as RETURNING must use this entry point.
func InspectProjection(root Projection, volatileFuncNames map[string]bool) SerializerInspection {
	inspection := &SerializerInspection{}
	visited := map[uintptr]bool{}
	inspectProjection(root, volatileFuncNames, visited, inspection)
	return *inspection
}

func inspectProjection(
	p Projection,
	volatileFuncNames map[string]bool,
	visited map[uintptr]bool,
	inspection *SerializerInspection,
) {
	if is.Nil(p) {
		return
	}

	switch projection := p.(type) {
	case *alias:
		inspectSerializer(projection.expression, volatileFuncNames, visited, inspection)
	case ProjectionList:
		for _, nested := range projection {
			inspectProjection(nested, volatileFuncNames, visited, inspection)
		}
	case Serializer:
		inspectSerializer(projection, volatileFuncNames, visited, inspection)
	default:
		// Unknown projection implementations cannot be inspected; treat them as opaque.
		inspection.HasOpaque = true
	}
}

// wrappedSerializer is implemented by every expression wrapper that simply forwards serialization
// to another expression. The method is promoted by all types embedding expressionWrapper.
type wrappedSerializer interface {
	unwrapWrappedExpression() Expression
}

func (e expressionWrapper) unwrapWrappedExpression() Expression {
	return e.expression
}

func inspectSerializer(
	s Serializer,
	volatileFuncNames map[string]bool,
	visited map[uintptr]bool,
	inspection *SerializerInspection,
) {
	if is.Nil(s) {
		return
	}

	if identity, ok := serializerIdentity(s); ok {
		if visited[identity] {
			return
		}
		visited[identity] = true
	}

	switch v := s.(type) {
	case *expression:
		inspectSerializer(v.Serializer, volatileFuncNames, visited, inspection)
		return

	case wrappedSerializer:
		inspectSerializer(v.unwrapWrappedExpression(), volatileFuncNames, visited, inspection)
		return

	case *rawSerializer:
		// Raw SQL text can contain arbitrary volatile or side-effecting calls.
		inspection.HasOpaque = true
		return

	case *customSerializer:
		for _, part := range v.parts {
			inspectSerializer(part, volatileFuncNames, visited, inspection)
		}
		return

	case *funcSerializer:
		name := strings.ToUpper(v.name)
		if volatileFuncNames[name] {
			inspection.VolatileFunctions = appendUnique(inspection.VolatileFunctions, name)
		}
		inspectSerializer(v.parameters, volatileFuncNames, visited, inspection)
		return

	case parametersSerializer:
		for _, expression := range v {
			inspectSerializer(expression, volatileFuncNames, visited, inspection)
		}
		return

	case *binaryOperatorSerializer:
		inspectSerializer(v.lhs, volatileFuncNames, visited, inspection)
		inspectSerializer(v.rhs, volatileFuncNames, visited, inspection)
		inspectSerializer(v.additionalParam, volatileFuncNames, visited, inspection)
		return

	case *betweenOperatorSerializer:
		inspectSerializer(v.expression, volatileFuncNames, visited, inspection)
		inspectSerializer(v.min, volatileFuncNames, visited, inspection)
		inspectSerializer(v.max, volatileFuncNames, visited, inspection)
		return

	case *serializersWithOperator:
		for _, expression := range v.serializers {
			inspectSerializer(expression, volatileFuncNames, visited, inspection)
		}
		return

	case *caseOperatorImpl:
		inspectSerializer(v.expression, volatileFuncNames, visited, inspection)
		for _, when := range v.when {
			inspectSerializer(when, volatileFuncNames, visited, inspection)
		}
		for _, then := range v.then {
			inspectSerializer(then, volatileFuncNames, visited, inspection)
		}
		inspectSerializer(v.els, volatileFuncNames, visited, inspection)
		return

	case *regexpLikeSerializer:
		inspectSerializer(v.str, volatileFuncNames, visited, inspection)
		inspectSerializer(v.pattern, volatileFuncNames, visited, inspection)
		return

	case columnAssigmentImpl:
		inspectSerializer(v.column, volatileFuncNames, visited, inspection)
		inspectSerializer(v.toAssign, volatileFuncNames, visited, inspection)
		return

	case columnListAssigment:
		for _, assignment := range v {
			inspectSerializer(assignment, volatileFuncNames, visited, inspection)
		}
		return

	case ListSerializer:
		for _, serializer := range v.Serializers {
			inspectSerializer(serializer, volatileFuncNames, visited, inspection)
		}
		return

	case *serializerImpl:
		for _, clause := range v.Clauses {
			inspectClause(clause, volatileFuncNames, visited, inspection)
		}
		return

	case *rowExpressionWrapper:
		inspectSerializer(v.Expression, volatileFuncNames, visited, inspection)
		for _, expression := range v.expressions {
			inspectSerializer(expression, volatileFuncNames, visited, inspection)
		}
		return

	case Values:
		for _, row := range v {
			inspectSerializer(row, volatileFuncNames, visited, inspection)
		}
		return

	case ColumnList:
		for _, column := range v {
			inspectSerializer(column, volatileFuncNames, visited, inspection)
		}
		return

	case JsonObjProjectionList:
		for _, projection := range v {
			inspectProjection(projection, volatileFuncNames, visited, inspection)
		}
		return

	case *windowImpl:
		inspectWindow(v, volatileFuncNames, visited, inspection)
		return

	case windowName:
		inspectWindow(&v.windowImpl, volatileFuncNames, visited, inspection)
		return

	case *frameExtentImpl:
		inspectSerializer(v.offset, volatileFuncNames, visited, inspection)
		return

	case *orderSetAggregateFuncSerializer:
		inspectSerializer(v.fraction, volatileFuncNames, visited, inspection)
		inspectSerializer(v.orderBy, volatileFuncNames, visited, inspection)
		return

	case *selectLockImpl:
		inspection.HasStatement = true
		return
	}

	// Marker interfaces for statements, sub-queries and tables.
	switch s.(type) {
	case Statement:
		inspection.HasStatement = true
		return
	case SelectTable:
		inspection.HasStatement = true
		return
	case SerializerTable:
		inspection.HasStatement = true
		return
	case RowLock:
		inspection.HasStatement = true
		return
	case Column:
		// Columns only render identifiers and never bind parameters.
		return
	}

	// Known leaf serializers that are guaranteed to be safe.
	switch s.(type) {
	case *literalSerializer:
		return
	case Keyword:
		return
	case Token:
		return
	case enumValueSerializer:
		return
	case KeywordClause:
		return
	case frameExtentKeyword:
		return
	}

	// Unknown serializer implementations cannot be inspected and are therefore treated as opaque.
	inspection.HasOpaque = true
}

func inspectClause(
	clause Clause,
	volatileFuncNames map[string]bool,
	visited map[uintptr]bool,
	inspection *SerializerInspection,
) {
	if is.Nil(clause) {
		return
	}

	switch c := clause.(type) {
	case KeywordClause:
		return
	case SetClauseNew:
		for _, assignment := range c {
			inspectSerializer(assignment, volatileFuncNames, visited, inspection)
		}
	case *ClauseWhere:
		inspectSerializer(c.Condition, volatileFuncNames, visited, inspection)
	case *ClauseReturning:
		for _, projection := range c.ProjectionList {
			inspectProjection(projection, volatileFuncNames, visited, inspection)
		}
	default:
		// Unknown clause kinds may carry arbitrary serializers.
		inspection.HasOpaque = true
	}
}

func inspectWindow(
	window *windowImpl,
	volatileFuncNames map[string]bool,
	visited map[uintptr]bool,
	inspection *SerializerInspection,
) {
	for _, expression := range window.partitionBy {
		inspectSerializer(expression, volatileFuncNames, visited, inspection)
	}

	for _, orderBy := range window.orderBy.List {
		if order, ok := orderBy.(*orderByClauseImpl); ok {
			inspectSerializer(order.expression, volatileFuncNames, visited, inspection)
		} else if !is.Nil(orderBy) {
			inspection.HasOpaque = true
		}
	}

	if !is.Nil(window.start) {
		inspectSerializer(window.start, volatileFuncNames, visited, inspection)
	}
	if !is.Nil(window.end) {
		inspectSerializer(window.end, volatileFuncNames, visited, inspection)
	}
}

// serializerIdentity returns a stable identity for reference-like serializers so that shared
// expression nodes are traversed only once. Value types have no stable identity and return false.
func serializerIdentity(s Serializer) (uintptr, bool) {
	value := reflect.ValueOf(s)

	switch value.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Slice, reflect.Map, reflect.Chan, reflect.Func:
		return value.Pointer(), true
	default:
		return 0, false
	}
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}

	return append(values, value)
}

// LiteralValue returns the bound value of a parameterized literal expression produced by Literal,
// including the literals created implicitly by VALUES/MODEL/MODELS for concrete Go values.
// It returns ok=false for DEFAULT/NULL keywords, fixed (inlined) literals and any other expression
// whose bound value cannot be determined statically.
func LiteralValue(s Serializer) (value interface{}, ok bool) {
	for !is.Nil(s) {
		switch v := s.(type) {
		case *expression:
			s = v.Serializer
		case wrappedSerializer:
			s = v.unwrapWrappedExpression()
		case *literalSerializer:
			if v.constant {
				return nil, false
			}
			return v.value, true
		default:
			return nil, false
		}
	}

	return nil, false
}
