package postgres

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/go-jet/jet/v2/internal/jet"
	"github.com/go-jet/jet/v2/internal/utils/is"
)

// InsertShard is one independently executable fragment of a split multi-row INSERT statement.
//
// A shard is a complete INSERT statement: it keeps the original column list, the whole
// ON CONFLICT clause and the RETURNING clause of the source statement, but only contains a
// contiguous range of the original input rows. Every shard numbers its bound-parameter
// placeholders independently, starting at $1.
//
// Shards are not executed as one atomic unit. A failure after some shards have already been
// executed leaves the database partially modified, and the result is not guaranteed to be
// identical to running the original single INSERT, in particular when the same conflict key
// appears in multiple input rows. When the source statement has a RETURNING clause, each shard
// only guarantees the order of the rows returned by that shard itself; there is no global,
// one-to-one correspondence between returned rows and input rows.
type InsertShard struct {
	// Statement is the independently serializable and executable INSERT fragment.
	Statement jet.SerializerStatement

	// RowStart is the zero-based, inclusive position of the first original input row in the shard.
	RowStart int

	// RowEnd is the zero-based, exclusive position immediately after the last row in the shard.
	RowEnd int

	// ParamCount is the exact number of bound parameters produced by Statement.Sql(),
	// measured with the serializer rather than estimated from rows multiplied by columns.
	ParamCount int
}

// SplitInsertOption configures InsertStatement splitting.
type SplitInsertOption func(*splitInsertSettings)

type splitInsertSettings struct {
	acceptShardingSemantics    bool
	allowVolatileExpressions   bool
	allowDuplicateConflictKeys bool
}

// AcceptShardingSemantics acknowledges that the generated shards are independent statements
// and are not atomically equivalent to the original single INSERT statement. This option is
// mandatory: splitting is refused until the caller explicitly accepts sharding semantics.
func AcceptShardingSemantics() SplitInsertOption {
	return func(settings *splitInsertSettings) {
		settings.acceptShardingSemantics = true
	}
}

// AllowVolatileExpressions permits opaque or potentially volatile expressions (Raw fragments,
// user provided Serializer implementations, embedded statements or known volatile functions such
// as random() or nextval()). Without this option such expressions make splitting fail, because
// their per-row evaluation may rely on single-statement semantics.
func AllowVolatileExpressions() SplitInsertOption {
	return func(settings *splitInsertSettings) {
		settings.allowVolatileExpressions = true
	}
}

// AllowDuplicateConflictKeys permits splitting when the same ON CONFLICT key is present in more
// than one input row. Without this option splitting fails in that case, because the order in which
// conflicting rows are applied across separate statements can change the final result.
func AllowDuplicateConflictKeys() SplitInsertOption {
	return func(settings *splitInsertSettings) {
		settings.allowDuplicateConflictKeys = true
	}
}

// volatileFunctionNames is the set of PostgreSQL function names that are known to be volatile or
// to have side effects, so splitting rows that call them requires an explicit opt-in.
var volatileFunctionNames = map[string]bool{
	"RANDOM":              true,
	"GEN_RANDOM_UUID":     true,
	"NEXTVAL":             true,
	"SETVAL":              true,
	"CURRVAL":             true,
	"LASTVAL":             true,
	"CLOCK_TIMESTAMP":     true,
	"STATEMENT_TIMESTAMP": true,
	"TIMEOFDAY":           true,
	"TXID_CURRENT":        true,
}

var placeholderRegexp = regexp.MustCompile(`\$(\d+)`)

// SplitInsertByParamBudget splits a multi-row INSERT statement built with VALUES, MODEL or MODELS
// into independently executable shards whose exact number of bound parameters does not exceed
// paramLimit. No SQL is executed and the source statement, its models and its expressions are
// never modified.
//
// Parameter counts are measured by serializing each row and each fixed clause with the regular
// serializer, so DEFAULT entries, inlined (fixed) literals, nested expressions and ON CONFLICT
// predicates are counted exactly instead of being estimated as rows multiplied by columns. Fixed
// parameters coming from ON CONFLICT or RETURNING are repeated in every shard and are charged to
// each shard's budget.
//
// A single input row is never split. If the fixed clauses together with one row already exceed
// paramLimit, the original position of that row is reported in the error and no plan is returned.
func SplitInsertByParamBudget(
	insert InsertStatement,
	paramLimit int,
	options ...SplitInsertOption,
) ([]InsertShard, error) {
	settings := &splitInsertSettings{}
	for _, option := range options {
		option(settings)
	}

	impl, ok := insert.(*insertStatementImpl)
	if !ok {
		return nil, errors.New("jet: SplitInsertByParamBudget: unsupported InsertStatement implementation")
	}

	if paramLimit <= 0 {
		return nil, fmt.Errorf("jet: SplitInsertByParamBudget: paramLimit must be greater than 0, got %d", paramLimit)
	}

	if impl.ValuesQuery.Query != nil {
		return nil, errors.New("jet: SplitInsertByParamBudget: INSERT ... QUERY statements cannot be split into row shards; only VALUES/MODEL/MODELS inserts are supported")
	}

	rows := impl.ValuesQuery.Rows
	if len(rows) == 0 {
		return nil, errors.New("jet: SplitInsertByParamBudget: insert statement has no VALUES rows to split")
	}

	if !settings.acceptShardingSemantics {
		return nil, errors.New("jet: SplitInsertByParamBudget: shards are independent statements and are not " +
			"atomically equivalent to the original single INSERT (partial failures and repeated ON CONFLICT keys " +
			"can change the result); pass AcceptShardingSemantics() to acknowledge these sharding semantics")
	}

	if err := validateExpressions(impl, settings); err != nil {
		return nil, err
	}

	if err := validateConflictKeys(impl, settings); err != nil {
		return nil, err
	}

	fixedParamCount := countSerializedParameters(impl.cloneWithRows(nil))

	if fixedParamCount > paramLimit {
		return nil, fmt.Errorf(
			"jet: SplitInsertByParamBudget: fixed clauses alone require %d parameters, which exceeds the parameter limit of %d",
			fixedParamCount, paramLimit)
	}

	rowParamCounts := make([]int, len(rows))
	totalRowParamCount := 0

	for rowIndex, row := range rows {
		rowParamCounts[rowIndex] = countRowParameters(row)
		totalRowParamCount += rowParamCounts[rowIndex]
	}

	// Invariant check: the exact count of the whole statement must equal the fixed overhead plus
	// the sum of the per-row counts. This guarantees no parameter-producing clause is overlooked.
	fullParamCount := countSerializedParameters(impl)
	if fullParamCount != fixedParamCount+totalRowParamCount {
		return nil, fmt.Errorf(
			"jet: SplitInsertByParamBudget: internal parameter count mismatch: full statement has %d parameters, fixed clauses %d, rows %d",
			fullParamCount, fixedParamCount, totalRowParamCount)
	}

	rowBudget := paramLimit - fixedParamCount

	// Validate every row before any shard is built so that an oversized row never leaves a partial plan.
	for rowIndex, rowParamCount := range rowParamCounts {
		if rowParamCount > rowBudget {
			return nil, fmt.Errorf(
				"jet: SplitInsertByParamBudget: input row %d alone requires %d parameters, but only %d parameters remain after the %d fixed-clause parameters under the limit of %d",
				rowIndex+1, rowParamCount, rowBudget, fixedParamCount, paramLimit)
		}
	}

	return buildShards(impl, rows, rowParamCounts, fixedParamCount, paramLimit)
}

func buildShards(
	impl *insertStatementImpl,
	rows [][]jet.Serializer,
	rowParamCounts []int,
	fixedParamCount int,
	paramLimit int,
) ([]InsertShard, error) {
	var shards []InsertShard

	shardStart := 0
	shardParamCount := fixedParamCount

	flush := func(rowEnd int) error {
		shardRows := append([][]jet.Serializer(nil), rows[shardStart:rowEnd]...)
		shardStatement := impl.cloneWithRows(shardRows)

		query, args := shardStatement.Sql()
		if len(args) != shardParamCount {
			return fmt.Errorf(
				"jet: SplitInsertByParamBudget: internal shard parameter mismatch for rows %d-%d: serialized %d parameters, planned %d",
				shardStart+1, rowEnd, len(args), shardParamCount)
		}

		if err := validatePlaceholders(query, len(args)); err != nil {
			return err
		}

		if shardParamCount > paramLimit {
			return fmt.Errorf(
				"jet: SplitInsertByParamBudget: internal budget violation for rows %d-%d: %d parameters exceed the limit of %d",
				shardStart+1, rowEnd, shardParamCount, paramLimit)
		}

		shards = append(shards, InsertShard{
			Statement:  shardStatement,
			RowStart:   shardStart,
			RowEnd:     rowEnd,
			ParamCount: shardParamCount,
		})

		return nil
	}

	for rowIndex := range rows {
		if rowIndex > shardStart && shardParamCount+rowParamCounts[rowIndex] > paramLimit {
			if err := flush(rowIndex); err != nil {
				return nil, err
			}

			shardStart = rowIndex
			shardParamCount = fixedParamCount
		}

		shardParamCount += rowParamCounts[rowIndex]
	}

	if err := flush(len(rows)); err != nil {
		return nil, err
	}

	return shards, nil
}

// validatePlaceholders ensures the rendered query numbers its placeholders contiguously from $1
// to $N, where N is the number of bound arguments.
func validatePlaceholders(query string, argCount int) error {
	matches := placeholderRegexp.FindAllStringSubmatch(query, -1)
	if len(matches) != argCount {
		return fmt.Errorf(
			"jet: SplitInsertByParamBudget: internal placeholder mismatch: query contains %d placeholders but %d arguments",
			len(matches), argCount)
	}

	for index, match := range matches {
		if match[1] != fmt.Sprintf("%d", index+1) {
			return fmt.Errorf(
				"jet: SplitInsertByParamBudget: internal placeholder numbering error: expected $%d, got %s",
				index+1, match[0])
		}
	}

	return nil
}

// cloneWithRows creates a new insert statement sharing the immutable clauses of the source but
// carrying only the provided rows. The source statement and its rows are never modified.
func (i *insertStatementImpl) cloneWithRows(rows [][]jet.Serializer) *insertStatementImpl {
	clone := &insertStatementImpl{}
	clone.SerializerStatement = jet.NewStatementImpl(Dialect, jet.InsertStatementType, clone,
		&clone.Insert,
		&clone.ValuesQuery,
		&clone.OnConflict,
		&clone.Returning,
	)

	clone.Insert = i.Insert
	clone.ValuesQuery.ClauseValues = jet.ClauseValues{Rows: rows}
	clone.Returning = i.Returning
	clone.OnConflict = i.OnConflict
	// The conflict clause holds a back-reference to its enclosing statement; repoint it to the clone.
	clone.OnConflict.insertStatement = clone

	return clone
}

func countSerializedParameters(serializer jet.Serializer) int {
	out := &jet.SQLBuilder{Dialect: Dialect}
	jet.Serialize(serializer, jet.InsertStatementType, out, jet.NoWrap)
	return len(out.Args)
}

func countRowParameters(row []jet.Serializer) int {
	out := &jet.SQLBuilder{Dialect: Dialect}
	(&jet.ClauseValues{Rows: [][]jet.Serializer{row}}).Serialize(jet.InsertStatementType, out)
	return len(out.Args)
}

type inspectionTarget struct {
	location   string
	inspection jet.SerializerInspection
}

func validateExpressions(impl *insertStatementImpl, settings *splitInsertSettings) error {
	if settings.allowVolatileExpressions {
		return nil
	}

	targets := make([]inspectionTarget, 0, len(impl.ValuesQuery.Rows)+2)

	for rowIndex, row := range impl.ValuesQuery.Rows {
		for _, cell := range row {
			targets = append(targets, inspectionTarget{
				location:   fmt.Sprintf("input row %d", rowIndex+1),
				inspection: jet.InspectSerializer(cell, volatileFunctionNames),
			})
		}
	}

	for _, serializer := range conflictClauseSerializers(&impl.OnConflict) {
		targets = append(targets, inspectionTarget{
			location:   "ON CONFLICT clause",
			inspection: jet.InspectSerializer(serializer, volatileFunctionNames),
		})
	}

	for _, projection := range impl.Returning.ProjectionList {
		targets = append(targets, inspectionTarget{
			location:   "RETURNING clause",
			inspection: jet.InspectProjection(projection, volatileFunctionNames),
		})
	}

	for _, target := range targets {
		if !target.inspection.RequiresExplicitOptIn() {
			continue
		}

		return fmt.Errorf(
			"jet: SplitInsertByParamBudget: %s contains an opaque, volatile or statement-dependent expression (%s); "+
				"pass AllowVolatileExpressions() to explicitly accept evaluating it independently in every shard",
			target.location, describeInspection(target.inspection))
	}

	return nil
}

func conflictClauseSerializers(onConflict *onConflictClause) []jet.Serializer {
	if is.Nil(onConflict.do) {
		return nil
	}

	var serializers []jet.Serializer

	for _, indexExpression := range onConflict.indexExpressions {
		serializers = append(serializers, indexExpression)
	}

	if onConflict.whereClause.Condition != nil {
		serializers = append(serializers, onConflict.whereClause.Condition)
	}

	switch action := onConflict.do.(type) {
	case jet.Keyword:
		// DO NOTHING renders a static keyword and has no nested expressions.
	case *updateConflictActionImpl:
		for _, assignment := range action.set {
			serializers = append(serializers, assignment)
		}

		if action.where.Condition != nil {
			serializers = append(serializers, action.where.Condition)
		}
	default:
		serializers = append(serializers, action)
	}

	return serializers
}

func describeInspection(inspection jet.SerializerInspection) string {
	var details []string

	if len(inspection.VolatileFunctions) > 0 {
		details = append(details, "volatile function(s): "+strings.Join(inspection.VolatileFunctions, ", "))
	}

	if inspection.HasOpaque {
		details = append(details, "raw or otherwise opaque SQL fragment")
	}

	if inspection.HasStatement {
		details = append(details, "embedded statement or sub-query")
	}

	if len(details) == 0 {
		return "unsplittable expression"
	}

	return strings.Join(details, "; ")
}

func validateConflictKeys(impl *insertStatementImpl, settings *splitInsertSettings) error {
	if settings.allowDuplicateConflictKeys {
		return nil
	}

	firstRow, secondRow, ok := impl.findDuplicateConflictKey()
	if !ok {
		return nil
	}

	return fmt.Errorf(
		"jet: SplitInsertByParamBudget: the same ON CONFLICT key appears in input rows %d and %d; applying the rows "+
			"in separate statements can change the result, pass AllowDuplicateConflictKeys() to accept this explicitly",
		firstRow, secondRow)
}

// findDuplicateConflictKey performs a best-effort detection of two input rows sharing the same
// ON CONFLICT key. It returns ok=false when the key cannot be proven statically (non-column index
// expressions or non-literal row values), in which case the generic sharding contract still applies.
func (i *insertStatementImpl) findDuplicateConflictKey() (firstRow int, secondRow int, ok bool) {
	if is.Nil(i.OnConflict.do) || len(i.OnConflict.indexExpressions) == 0 {
		return 0, 0, false
	}

	columns := i.Insert.GetColumns()
	keyPositions := make([]int, 0, len(i.OnConflict.indexExpressions))

	for _, indexExpression := range i.OnConflict.indexExpressions {
		position := -1

		for columnIndex, column := range columns {
			if column.Name() == indexExpression.Name() {
				position = columnIndex
				break
			}
		}

		if position == -1 {
			return 0, 0, false
		}

		keyPositions = append(keyPositions, position)
	}

	seenKeys := map[string]int{}

	for rowIndex, row := range i.ValuesQuery.Rows {
		keyValues := make([]interface{}, 0, len(keyPositions))

		for _, position := range keyPositions {
			if position >= len(row) {
				return 0, 0, false
			}

			value, isLiteral := jet.LiteralValue(row[position])
			if !isLiteral {
				return 0, 0, false
			}

			keyValues = append(keyValues, value)
		}

		key := fmt.Sprintf("%#v", keyValues)
		if previousRow, exists := seenKeys[key]; exists {
			return previousRow + 1, rowIndex + 1, true
		}

		seenKeys[key] = rowIndex
	}

	return 0, 0, false
}
