// Static, database independent reference checking for serialized statements.
//
// The checked entry validates the table/column reference domain of a statement using the structured
// query builder objects only. It never connects to a database, never rewrites references, never
// mutates shared table or column objects and never auto-corrects invalid aliases. It does not prove
// expression equivalence and it does not perform database column type checking: when the column set
// of a relation is not statically known, column membership is left unproven instead of being rejected.
//
// Raw fragments (Raw, RawStatement) and serializer implementations that the checker cannot traverse
// are reported as opaque findings. They never count as a successful check: the caller has to choose
// explicitly between rejecting and accepting them through an OpaquePolicy.

package jet

import (
	"strings"

	"github.com/go-jet/jet/v2/internal/utils/is"
)

// OpaquePolicy decides how checked serialization treats statements containing opaque nodes
// (Raw fragments or custom serializers that cannot be traversed).
type OpaquePolicy int

const (
	// RejectOpaque is the default policy. A statement containing any opaque node is rejected with
	// a CheckedError. The caller has to opt into AcceptOpaque explicitly, so an unchecked statement
	// can never be silently reported as fully checked.
	RejectOpaque OpaquePolicy = iota
	// AcceptOpaque allows serialization to succeed when the only findings are opaque nodes. The
	// returned report keeps status StatusOpaque and lists every opaque location. Reference errors
	// are still rejected.
	AcceptOpaque
)

// CheckedOption customizes checked serialization.
type CheckedOption func(*checkedConfig)

type checkedConfig struct {
	opaquePolicy OpaquePolicy
}

// WithOpaquePolicy sets the policy applied to opaque (non traversable) statement parts.
func WithOpaquePolicy(policy OpaquePolicy) CheckedOption {
	return func(cfg *checkedConfig) {
		cfg.opaquePolicy = policy
	}
}

// CheckedStatus is the overall result of the static check.
type CheckedStatus string

const (
	// StatusChecked means every traversed reference was validated and no opaque node was found.
	StatusChecked CheckedStatus = "checked"
	// StatusOpaque means no reference error was found, but at least one opaque node prevented a
	// complete check. This status only occurs with AcceptOpaque.
	StatusOpaque CheckedStatus = "opaque"
	// StatusInvalid means at least one reference domain error was detected.
	StatusInvalid CheckedStatus = "invalid"
)

// ReferenceErrorKind classifies a static reference domain error.
type ReferenceErrorKind string

const (
	// ErrUnknownRelation is reported when a qualified column references a table alias (or table name)
	// that is not visible at the reference location.
	ErrUnknownRelation ReferenceErrorKind = "unknown_relation"
	// ErrUnknownColumn is reported when a column cannot be found in the known columns of its relation.
	// Relations whose columns are not statically known are never used to produce this error.
	ErrUnknownColumn ReferenceErrorKind = "unknown_column"
	// ErrAmbiguousColumn is reported when an unqualified column name matches more than one relation.
	ErrAmbiguousColumn ReferenceErrorKind = "ambiguous_column"
	// ErrDuplicateRelation is reported when the same relation name is introduced twice in the same
	// query block. Reusing an outer relation name in a nested block (shadowing) is legal and handled
	// as a separate inner relation, not as an error.
	ErrDuplicateRelation ReferenceErrorKind = "duplicate_relation"
	// ErrIllegalCorrelation is reported when a non LATERAL derived table references another FROM
	// item of the same query block. Only outer query block relations may be correlated.
	ErrIllegalCorrelation ReferenceErrorKind = "illegal_correlation"
	// ErrLateralBoundary is reported when a LATERAL derived table references a FROM item positioned
	// to its right. LATERAL may only reference relations introduced on its left.
	ErrLateralBoundary ReferenceErrorKind = "lateral_boundary_violation"
	// ErrOpaqueNode is reported when an opaque node is encountered under RejectOpaque.
	ErrOpaqueNode ReferenceErrorKind = "opaque_node"
	// ErrUnsupportedStatement is reported when the root statement is outside the supported range
	// (currently SELECT, set operations, WITH/recursive CTE and INSERT ... QUERY/VALUES).
	ErrUnsupportedStatement ReferenceErrorKind = "unsupported_statement"
)

// ReferenceError describes a single reference domain violation.
type ReferenceError struct {
	// Kind classifies the violation.
	Kind ReferenceErrorKind
	// Path is the structured location of the offending reference inside the statement, for
	// example "with.cte[1].body.from[0].on" or "select.where".
	Path string
	// Relation is the qualified relation name (alias or table name) involved, when applicable.
	Relation string
	// Column is the atomic column identifier involved, when applicable. The identifier is never
	// split on dots or case folded.
	Column string
	// Message is a human readable description of the violation.
	Message string
}

// Error implements the error interface.
func (e *ReferenceError) Error() string {
	var builder strings.Builder

	builder.WriteString("jet: checked reference error: ")
	builder.WriteString(string(e.Kind))
	if e.Path != "" {
		builder.WriteString(" at ")
		builder.WriteString(e.Path)
	}
	if e.Message != "" {
		builder.WriteString(": ")
		builder.WriteString(e.Message)
	}

	return builder.String()
}

// OpaqueKind classifies an opaque node.
type OpaqueKind string

const (
	// OpaqueRaw marks a jet Raw expression.
	OpaqueRaw OpaqueKind = "raw_expression"
	// OpaqueRawStatement marks a raw statement (RawStatement).
	OpaqueRawStatement OpaqueKind = "raw_statement"
	// OpaqueSerializerFunc marks a function based serializer whose contents cannot be inspected.
	OpaqueSerializerFunc OpaqueKind = "serializer_func"
	// OpaqueCustom marks any other serializer implementation unknown to the checker.
	OpaqueCustom OpaqueKind = "custom_serializer"
)

// OpaqueFinding records the structured location of a statement part that cannot be checked.
type OpaqueFinding struct {
	// Path is the structured location of the opaque node inside the statement.
	Path string
	// Kind classifies the opaque node.
	Kind OpaqueKind
	// Detail describes why the node cannot be traversed.
	Detail string
}

// CheckedReport collects every finding produced by a static check. It never contains panics or
// database errors: only reference domain errors and opaque nodes are reported here.
type CheckedReport struct {
	Errors []ReferenceError
	Opaque []OpaqueFinding
}

// Status derives the overall check status from the findings.
func (r *CheckedReport) Status() CheckedStatus {
	switch {
	case len(r.Errors) > 0:
		return StatusInvalid
	case len(r.Opaque) > 0:
		return StatusOpaque
	default:
		return StatusChecked
	}
}

// CheckedError wraps a report whose status does not allow checked serialization to succeed.
type CheckedError struct {
	Report *CheckedReport
}

// Error implements the error interface.
func (e *CheckedError) Error() string {
	if e == nil || e.Report == nil {
		return "jet: checked serialization failed"
	}

	var builder strings.Builder

	builder.WriteString("jet: checked serialization failed (status ")
	builder.WriteString(string(e.Report.Status()))
	builder.WriteString(")")

	for _, refErr := range e.Report.Errors {
		builder.WriteString("\n - ")
		builder.WriteString(refErr.Error())
	}

	for _, opaque := range e.Report.Opaque {
		builder.WriteString("\n - opaque ")
		builder.WriteString(string(opaque.Kind))
		if opaque.Path != "" {
			builder.WriteString(" at ")
			builder.WriteString(opaque.Path)
		}
		if opaque.Detail != "" {
			builder.WriteString(": ")
			builder.WriteString(opaque.Detail)
		}
	}

	return builder.String()
}

// CheckedResult is the successful output of checked serialization.
type CheckedResult struct {
	// SQL is the parameterized query, identical to what Statement.Sql would produce.
	SQL string
	// Args is the parameterized argument list, identical to what Statement.Sql would produce.
	Args []interface{}
	// Report describes the check. Its status is StatusChecked or, under AcceptOpaque, StatusOpaque.
	Report *CheckedReport
}

// CheckedUnwrapper is implemented by dialect level wrapper serializers that hide an internal jet
// serializer. The checker unwraps such objects instead of treating them as opaque custom nodes.
type CheckedUnwrapper interface {
	CheckedUnwrap() Serializer
}

// CheckedNode is a named child node exposed by a dialect specific clause that cannot be inspected
// from the internal jet package.
type CheckedNode struct {
	// Name is the stable slot name used to build the structured error path.
	Name string
	// Node is the child serializer to check.
	Node Serializer
}

// CheckedNodeProvider is implemented by dialect specific clauses that want their named children to
// be traversed during a checked run.
type CheckedNodeProvider interface {
	CheckedNodes() []CheckedNode
}

// CheckedSerialize runs the static reference check against statement and, when it passes, serializes
// the statement using its regular, unchanged Sql implementation. The check is read only.
func CheckedSerialize(statement SerializerStatement, options ...CheckedOption) (*CheckedResult, error) {
	cfg := checkedConfig{opaquePolicy: RejectOpaque}

	for _, option := range options {
		option(&cfg)
	}

	report := newReferenceChecker().checkRoot(statement)

	// Under the default reject policy every opaque finding is surfaced as an explicit error as
	// well, so a partially opaque statement is never implicitly reported as checked.
	if cfg.opaquePolicy == RejectOpaque {
		for _, finding := range report.Opaque {
			report.Errors = append(report.Errors, ReferenceError{
				Kind:    ErrOpaqueNode,
				Path:    finding.Path,
				Message: string(finding.Kind) + ": " + finding.Detail,
			})
		}
	}

	if len(report.Errors) > 0 {
		return nil, &CheckedError{Report: report}
	}

	query, args := statement.Sql()

	return &CheckedResult{
		SQL:    query,
		Args:   args,
		Report: report,
	}, nil
}

// maxUnwrapDepth bounds wrapper chains so a misbehaving CheckedUnwrap implementation cannot loop
// forever. Wrapper structs in the built in dialects are only a few levels deep.
const maxUnwrapDepth = 32

// checkedUnwrap repeatedly removes dialect wrapper serializers.
func checkedUnwrap(node Serializer) Serializer {
	for depth := 0; depth < maxUnwrapDepth; depth++ {
		if is.Nil(node) {
			return node
		}

		unwrapper, ok := node.(CheckedUnwrapper)
		if !ok {
			return node
		}

		inner := unwrapper.CheckedUnwrap()
		if is.Nil(inner) {
			return inner
		}

		node = inner
	}

	return node
}

// columnRefInfo is the structured, atomic qualified identity of a column reference. Names are kept
// exactly as declared: they are never split on dots nor case folded.
type columnRefInfo struct {
	qualifier string
	column    string
}

// columnRefProvider is implemented by every column expression through the embedded
// ColumnExpressionImpl. ColumnList implements it as well but is always handled as a container before
// the provider is consulted.
type columnRefProvider interface {
	columnRefInfo() columnRefInfo
}

func (c *ColumnExpressionImpl) columnRefInfo() columnRefInfo {
	if c.subQuery != nil {
		return columnRefInfo{
			qualifier: c.subQuery.Alias(),
			column:    c.defaultAlias(),
		}
	}

	return columnRefInfo{
		qualifier: c.tableName,
		column:    c.name,
	}
}

func (cl ColumnList) columnRefInfo() columnRefInfo {
	return columnRefInfo{}
}
