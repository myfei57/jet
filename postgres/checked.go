// Explicit checked serialization entry for PostgreSQL statements.
//
// Checked runs a static, database independent reference check over a statement and, when the check
// passes, returns the same parameterized SQL/args pair as Statement.Sql. The regular Sql entry is
// never altered: checked serialization is opt in and fully additive.
//
// The check only reasons about the structured query builder objects (table/column provenance,
// aliases, query block scopes, LATERAL boundaries, CTE visibility and SELECT output aliases). It
// does not connect to a database, does not perform column type checking, does not try to prove
// expression equivalence and never rewrites or "repairs" a reference. Raw fragments and custom
// serializers that cannot be traversed are reported as opaque nodes: callers must explicitly choose
// between rejecting and accepting them through an OpaquePolicy.

package postgres

import (
	"strconv"

	"github.com/go-jet/jet/v2/internal/jet"
	"github.com/go-jet/jet/v2/internal/utils/is"
)

// Re-exported checked serialization API.
type (
	// OpaquePolicy decides how opaque (non traversable) statement parts are handled.
	OpaquePolicy = jet.OpaquePolicy
	// CheckedOption customizes checked serialization.
	CheckedOption = jet.CheckedOption
	// CheckedResult is the successful output of checked serialization.
	CheckedResult = jet.CheckedResult
	// CheckedReport collects every finding produced by a static check.
	CheckedReport = jet.CheckedReport
	// CheckedStatus is the overall result of the static check.
	CheckedStatus = jet.CheckedStatus
	// ReferenceError describes a single reference domain violation.
	ReferenceError = jet.ReferenceError
	// ReferenceErrorKind classifies a static reference domain error.
	ReferenceErrorKind = jet.ReferenceErrorKind
	// OpaqueFinding records the structured location of a non traversable statement part.
	OpaqueFinding = jet.OpaqueFinding
	// OpaqueKind classifies an opaque node.
	OpaqueKind = jet.OpaqueKind
	// CheckedError wraps a report whose status does not allow checked serialization to succeed.
	CheckedError = jet.CheckedError
)

// Opaque policies.
const (
	// RejectOpaque is the default policy: any opaque node makes checked serialization fail.
	RejectOpaque OpaquePolicy = jet.RejectOpaque
	// AcceptOpaque allows serialization to succeed when only opaque findings are present. The
	// returned report keeps the opaque status and lists every opaque location.
	AcceptOpaque OpaquePolicy = jet.AcceptOpaque
)

// Checked statuses.
const (
	StatusChecked CheckedStatus = jet.StatusChecked
	StatusOpaque  CheckedStatus = jet.StatusOpaque
	StatusInvalid CheckedStatus = jet.StatusInvalid
)

// Reference error kinds.
const (
	ErrUnknownRelation      ReferenceErrorKind = jet.ErrUnknownRelation
	ErrUnknownColumn        ReferenceErrorKind = jet.ErrUnknownColumn
	ErrAmbiguousColumn      ReferenceErrorKind = jet.ErrAmbiguousColumn
	ErrDuplicateRelation    ReferenceErrorKind = jet.ErrDuplicateRelation
	ErrIllegalCorrelation   ReferenceErrorKind = jet.ErrIllegalCorrelation
	ErrLateralBoundary      ReferenceErrorKind = jet.ErrLateralBoundary
	ErrOpaqueNode           ReferenceErrorKind = jet.ErrOpaqueNode
	ErrUnsupportedStatement ReferenceErrorKind = jet.ErrUnsupportedStatement
)

// Opaque node kinds.
const (
	OpaqueRaw            OpaqueKind = jet.OpaqueRaw
	OpaqueRawStatement   OpaqueKind = jet.OpaqueRawStatement
	OpaqueSerializerFunc OpaqueKind = jet.OpaqueSerializerFunc
	OpaqueCustom         OpaqueKind = jet.OpaqueCustom
)

// WithOpaquePolicy sets the policy applied to opaque statement parts.
func WithOpaquePolicy(policy OpaquePolicy) CheckedOption {
	return jet.WithOpaquePolicy(policy)
}

// Checked runs the static reference check over statement and, when the check passes, serializes
// the statement with its regular, unchanged Sql implementation. The default policy is
// RejectOpaque, so opaque nodes make the call fail unless AcceptOpaque is selected explicitly.
// The check is read only: it never mutates the statement, its tables or its column objects.
func Checked(statement jet.SerializerStatement, options ...CheckedOption) (*CheckedResult, error) {
	return jet.CheckedSerialize(statement, options...)
}

// ---------------------------------------------------------------------
// Dialect wrapper adapters. They expose the internal jet object hidden by each postgres wrapper
// so the checker can traverse the structured query instead of treating the wrapper as opaque.
// ---------------------------------------------------------------------

func (t *tableImpl) CheckedUnwrap() jet.Serializer { return t.SerializerTable }

func (t *joinTable) CheckedUnwrap() jet.Serializer { return t.JoinTable }

func (t *selectTableImpl) CheckedUnwrap() jet.Serializer { return t.SelectTable }

func (c *commonTableExpression) CheckedUnwrap() jet.Serializer { return c.CommonTableExpression }

func (v values) CheckedUnwrap() jet.Serializer { return v.Values }

func (s *selectStatementImpl) CheckedUnwrap() jet.Serializer { return s.ExpressionStatement }

func (s *selectJsonStatement) CheckedUnwrap() jet.Serializer { return s.selectStatementImpl }

func (s *setStatementImpl) CheckedUnwrap() jet.Serializer { return s.ExpressionStatement }

func (i *insertStatementImpl) CheckedUnwrap() jet.Serializer { return i.SerializerStatement }

func (u *updateStatementImpl) CheckedUnwrap() jet.Serializer { return u.SerializerStatement }

func (d *deleteStatementImpl) CheckedUnwrap() jet.Serializer { return d.SerializerStatement }

func (l *lockStatementImpl) CheckedUnwrap() jet.Serializer { return l.SerializerStatement }

func (u *updateConflictActionImpl) CheckedUnwrap() jet.Serializer { return u.Serializer }

// CheckedNodes exposes the traversable children of the dialect specific ON CONFLICT clause.
// The "action" node is checked with the EXCLUDED pseudo relation in scope, the other nodes with
// only the insert target relation.
func (o *onConflictClause) CheckedNodes() []jet.CheckedNode {
	var nodes []jet.CheckedNode

	for i, indexExpression := range o.indexExpressions {
		nodes = append(nodes, jet.CheckedNode{
			Name: indexPathName("target", i),
			Node: indexExpression,
		})
	}

	if !is.Nil(o.whereClause.Condition) {
		nodes = append(nodes, jet.CheckedNode{
			Name: "where",
			Node: o.whereClause.Condition,
		})
	}

	if !is.Nil(o.do) {
		nodes = append(nodes, jet.CheckedNode{
			Name: "action",
			Node: o.do,
		})
	}

	return nodes
}

func indexPathName(name string, index int) string {
	return name + "[" + strconv.Itoa(index) + "]"
}
