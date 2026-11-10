package jet

type rawStatementImpl struct {
	statementInterfaceImpl

	RawQuery       string
	NamedArguments map[string]interface{}
}

// RawStatement creates new sql statements from raw query and optional map of named arguments
func RawStatement(dialect Dialect, rawQuery string, namedArgument ...map[string]interface{}) SerializerStatement {
	newRawStatement := rawStatementImpl{
		statementInterfaceImpl: statementInterfaceImpl{
			dialect:       dialect,
			statementType: "",
			root:          nil,
		},
		RawQuery:       rawQuery,
		NamedArguments: singleOptional(namedArgument),
	}

	newRawStatement.root = &newRawStatement

	return &newRawStatement
}

func (s *rawStatementImpl) projections() ProjectionList {
	return nil
}

func (s *rawStatementImpl) serialize(statement StatementType, out *SQLBuilder, options ...SerializeOption) {
	if !contains(options, NoWrap) {
		out.WriteString("(")
		out.IncreaseIdent()
	}

	out.insertRawQuery(s.RawQuery, s.NamedArguments)

	if !contains(options, NoWrap) {
		out.DecreaseIdent()
		out.NewLine()
		out.WriteString(")")
	}
}

// IsRawExpression reports whether expr is an expression built with Raw or one of its typed
// helpers (RawBool, RawInt, RawString, ...). Typed expression wrappers produced, for instance,
// by StringExp(Raw(...)) are unwrapped before inspecting the underlying serializer.
//
// Columns, literals and other library-built expressions (functions, operators, casts, CASE, ...)
// are not considered raw expressions.
func IsRawExpression(expr Expression) bool {
	for expr != nil {
		switch e := expr.(type) {
		case *expression:
			_, ok := e.Serializer.(*rawSerializer)
			return ok
		case interface{ wrappedExpression() Expression }:
			expr = e.wrappedExpression()
		default:
			return false
		}
	}

	return false
}
