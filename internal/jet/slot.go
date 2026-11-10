package jet

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-jet/jet/v2/internal/utils/is"
	"github.com/go-jet/jet/v2/qrm"
)

// slotKind identifies the value type accepted by a slot. It is used only for binding validation;
// SQL serialization is identical for every kind, because the value itself is never read at compile time.
type slotKind int

const (
	slotKindInt slotKind = iota
	slotKindFloat
	slotKindBool
	slotKindString
	slotKindTime
	slotKindBlob
)

func (k slotKind) String() string {
	switch k {
	case slotKindInt:
		return "integer"
	case slotKindFloat:
		return "float"
	case slotKindBool:
		return "boolean"
	case slotKindString:
		return "string"
	case slotKindTime:
		return "time"
	case slotKindBlob:
		return "binary"
	default:
		return "unknown"
	}
}

// accepts reports whether value is one of the native Go types allowed for the slot kind.
// Values implementing driver.Valuer are checked separately by the caller and are always accepted,
// because the database driver is responsible for their conversion.
func (k slotKind) accepts(value any) bool {
	switch k {
	case slotKindInt:
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		}
	case slotKindFloat:
		switch value.(type) {
		case float32, float64:
			return true
		}
	case slotKindBool:
		_, ok := value.(bool)
		return ok
	case slotKindString:
		_, ok := value.(string)
		return ok
	case slotKindTime:
		_, ok := value.(time.Time)
		return ok
	case slotKindBlob:
		switch value.(type) {
		case []byte, string:
			return true
		}
	}

	return false
}

// slotBase holds the identity and the static declaration of a slot. Slots are distinguished by
// object identity (the slotBase pointer), never by name: two slots with the same name remain
// independent and are never merged.
type slotBase struct {
	name     string
	kind     slotKind
	nullable bool

	// expression is the typed slot expression wrapping this base. It allows the compiled
	// statement to expose the original slot objects through CompiledStatement.Slots.
	expression Slot
}

func newSlotBase(name string, kind slotKind, nullable bool) *slotBase {
	return &slotBase{name: name, kind: kind, nullable: nullable}
}

// SlotName returns the stable diagnostic name of the slot. The name is never used to resolve
// bindings or to deduplicate slots; it only appears in error messages and CompiledStatement.Slots.
func (s *slotBase) SlotName() string {
	return s.name
}

// base returns the identity holder of the slot. It is the unexported hook used by Compile/Bind to
// resolve slots by object identity; external callers only see the typed slot expression.
func (s *slotBase) base() *slotBase {
	return s
}

// serialize is promoted to every concrete slot type. It never reads a slot value, because slots
// do not hold values: the value is supplied later through CompiledStatement.Bind.
func (s *slotBase) serialize(statement StatementType, out *SQLBuilder, options ...SerializeOption) {
	out.registerSlot(s)
}

// Slot is a reusable, typed parameter placeholder. A slot is constructed once, can be embedded any
// number of times into a query (including CTEs, subqueries and the outer statement), and receives a
// value only when a compiled statement is bound. A slot must not be serialized through a plain
// Statement.Sql call; it can only be used through Statement.Compile and CompiledStatement.Bind.
type Slot interface {
	Expression

	// SlotName returns the stable diagnostic name of the slot.
	SlotName() string
	base() *slotBase
}

// Typed slot interfaces. Each one combines the corresponding typed expression with the Slot marker,
// so slots can be used anywhere a typed expression is expected while remaining identifiable at bind time.
type (
	// IntegerSlot is a slot accepting integer values.
	IntegerSlot interface {
		IntegerExpression
		Slot
	}

	// FloatSlot is a slot accepting float values.
	FloatSlot interface {
		FloatExpression
		Slot
	}

	// BoolSlot is a slot accepting boolean values.
	BoolSlot interface {
		BoolExpression
		Slot
	}

	// StringSlot is a slot accepting string values.
	StringSlot interface {
		StringExpression
		Slot
	}

	// DateSlot is a slot accepting date values.
	DateSlot interface {
		DateExpression
		Slot
	}

	// TimeSlot is a slot accepting time values.
	TimeSlot interface {
		TimeExpression
		Slot
	}

	// TimezSlot is a slot accepting time with time zone values.
	TimezSlot interface {
		TimezExpression
		Slot
	}

	// TimestampSlot is a slot accepting timestamp values.
	TimestampSlot interface {
		TimestampExpression
		Slot
	}

	// TimestampzSlot is a slot accepting timestamp with time zone values.
	TimestampzSlot interface {
		TimestampzExpression
		Slot
	}

	// BlobSlot is a slot accepting binary ([]byte) values.
	BlobSlot interface {
		BlobExpression
		Slot
	}
)

type integerSlot struct {
	ExpressionInterfaceImpl
	integerInterfaceImpl
	*slotBase
}

type floatSlot struct {
	ExpressionInterfaceImpl
	floatInterfaceImpl
	*slotBase
}

type boolSlot struct {
	ExpressionInterfaceImpl
	boolInterfaceImpl
	*slotBase
}

type stringSlot struct {
	ExpressionInterfaceImpl
	stringInterfaceImpl
	*slotBase
}

type dateSlot struct {
	ExpressionInterfaceImpl
	dateInterfaceImpl
	*slotBase
}

type timeSlot struct {
	ExpressionInterfaceImpl
	timeInterfaceImpl
	*slotBase
}

type timezSlot struct {
	ExpressionInterfaceImpl
	timezInterfaceImpl
	*slotBase
}

type timestampSlot struct {
	ExpressionInterfaceImpl
	timestampInterfaceImpl
	*slotBase
}

type timestampzSlot struct {
	ExpressionInterfaceImpl
	timestampzInterfaceImpl
	*slotBase
}

type blobSlot struct {
	ExpressionInterfaceImpl
	blobInterfaceImpl
	*slotBase
}

func newIntegerSlot(name string, nullable bool) IntegerSlot {
	slot := &integerSlot{slotBase: newSlotBase(name, slotKindInt, nullable)}
	slot.ExpressionInterfaceImpl.Root = slot
	slot.integerInterfaceImpl.root = slot
	slot.slotBase.expression = slot
	return slot
}

func newFloatSlot(name string, nullable bool) FloatSlot {
	slot := &floatSlot{slotBase: newSlotBase(name, slotKindFloat, nullable)}
	slot.ExpressionInterfaceImpl.Root = slot
	slot.floatInterfaceImpl.root = slot
	slot.slotBase.expression = slot
	return slot
}

func newBoolSlot(name string, nullable bool) BoolSlot {
	slot := &boolSlot{slotBase: newSlotBase(name, slotKindBool, nullable)}
	slot.ExpressionInterfaceImpl.Root = slot
	slot.boolInterfaceImpl.root = slot
	slot.slotBase.expression = slot
	return slot
}

func newStringSlot(name string, nullable bool) StringSlot {
	slot := &stringSlot{slotBase: newSlotBase(name, slotKindString, nullable)}
	slot.ExpressionInterfaceImpl.Root = slot
	slot.stringInterfaceImpl.root = slot
	slot.slotBase.expression = slot
	return slot
}

func newDateSlot(name string, nullable bool) DateSlot {
	slot := &dateSlot{slotBase: newSlotBase(name, slotKindTime, nullable)}
	slot.ExpressionInterfaceImpl.Root = slot
	slot.dateInterfaceImpl.root = slot
	slot.slotBase.expression = slot
	return slot
}

func newTimeSlot(name string, nullable bool) TimeSlot {
	slot := &timeSlot{slotBase: newSlotBase(name, slotKindTime, nullable)}
	slot.ExpressionInterfaceImpl.Root = slot
	slot.timeInterfaceImpl.root = slot
	slot.slotBase.expression = slot
	return slot
}

func newTimezSlot(name string, nullable bool) TimezSlot {
	slot := &timezSlot{slotBase: newSlotBase(name, slotKindTime, nullable)}
	slot.ExpressionInterfaceImpl.Root = slot
	slot.timezInterfaceImpl.root = slot
	slot.slotBase.expression = slot
	return slot
}

func newTimestampSlot(name string, nullable bool) TimestampSlot {
	slot := &timestampSlot{slotBase: newSlotBase(name, slotKindTime, nullable)}
	slot.ExpressionInterfaceImpl.Root = slot
	slot.timestampInterfaceImpl.root = slot
	slot.slotBase.expression = slot
	return slot
}

func newTimestampzSlot(name string, nullable bool) TimestampzSlot {
	slot := &timestampzSlot{slotBase: newSlotBase(name, slotKindTime, nullable)}
	slot.ExpressionInterfaceImpl.Root = slot
	slot.timestampzInterfaceImpl.root = slot
	slot.slotBase.expression = slot
	return slot
}

func newBlobSlot(name string, nullable bool) BlobSlot {
	slot := &blobSlot{slotBase: newSlotBase(name, slotKindBlob, nullable)}
	slot.ExpressionInterfaceImpl.Root = slot
	slot.blobInterfaceImpl.root = slot
	slot.slotBase.expression = slot
	return slot
}

// Slot constructors. Slots created with the plain constructors reject NULL on bind; slots created
// with the NewNullable* constructors explicitly allow nil (SQL NULL).

// NewIntSlot creates a named integer slot that does not accept NULL.
func NewIntSlot(name string) IntegerSlot { return newIntegerSlot(name, false) }

// NewNullableIntSlot creates a named integer slot that accepts NULL.
func NewNullableIntSlot(name string) IntegerSlot { return newIntegerSlot(name, true) }

// NewFloatSlot creates a named float slot that does not accept NULL.
func NewFloatSlot(name string) FloatSlot { return newFloatSlot(name, false) }

// NewNullableFloatSlot creates a named float slot that accepts NULL.
func NewNullableFloatSlot(name string) FloatSlot { return newFloatSlot(name, true) }

// NewBoolSlot creates a named boolean slot that does not accept NULL.
func NewBoolSlot(name string) BoolSlot { return newBoolSlot(name, false) }

// NewNullableBoolSlot creates a named boolean slot that accepts NULL.
func NewNullableBoolSlot(name string) BoolSlot { return newBoolSlot(name, true) }

// NewStringSlot creates a named string slot that does not accept NULL.
func NewStringSlot(name string) StringSlot { return newStringSlot(name, false) }

// NewNullableStringSlot creates a named string slot that accepts NULL.
func NewNullableStringSlot(name string) StringSlot { return newStringSlot(name, true) }

// NewDateSlot creates a named date slot that does not accept NULL.
func NewDateSlot(name string) DateSlot { return newDateSlot(name, false) }

// NewNullableDateSlot creates a named date slot that accepts NULL.
func NewNullableDateSlot(name string) DateSlot { return newDateSlot(name, true) }

// NewTimeSlot creates a named time slot that does not accept NULL.
func NewTimeSlot(name string) TimeSlot { return newTimeSlot(name, false) }

// NewNullableTimeSlot creates a named time slot that accepts NULL.
func NewNullableTimeSlot(name string) TimeSlot { return newTimeSlot(name, true) }

// NewTimezSlot creates a named time-with-time-zone slot that does not accept NULL.
func NewTimezSlot(name string) TimezSlot { return newTimezSlot(name, false) }

// NewNullableTimezSlot creates a named time-with-time-zone slot that accepts NULL.
func NewNullableTimezSlot(name string) TimezSlot { return newTimezSlot(name, true) }

// NewTimestampSlot creates a named timestamp slot that does not accept NULL.
func NewTimestampSlot(name string) TimestampSlot { return newTimestampSlot(name, false) }

// NewNullableTimestampSlot creates a named timestamp slot that accepts NULL.
func NewNullableTimestampSlot(name string) TimestampSlot { return newTimestampSlot(name, true) }

// NewTimestampzSlot creates a named timestamp-with-time-zone slot that does not accept NULL.
func NewTimestampzSlot(name string) TimestampzSlot { return newTimestampzSlot(name, false) }

// NewNullableTimestampzSlot creates a named timestamp-with-time-zone slot that accepts NULL.
func NewNullableTimestampzSlot(name string) TimestampzSlot { return newTimestampzSlot(name, true) }

// NewBlobSlot creates a named binary slot that does not accept NULL.
func NewBlobSlot(name string) BlobSlot { return newBlobSlot(name, false) }

// NewNullableBlobSlot creates a named binary slot that accepts NULL.
func NewNullableBlobSlot(name string) BlobSlot { return newBlobSlot(name, true) }

// SlotBinding pairs a slot with the value to bind to it. Instances are created with SlotValue.
type SlotBinding struct {
	slot  Slot
	value any
}

// SlotValue creates a SlotBinding for slot. Pass nil to bind SQL NULL to a nullable slot.
// The value is validated against the slot declaration when CompiledStatement.Bind is called.
func SlotValue(slot Slot, value any) SlotBinding {
	return SlotBinding{slot: slot, value: value}
}

// paramSpec describes a single frozen parameter position of a compiled statement.
// Either slot is set (the argument is resolved from a binding at Bind time) or value holds
// an ordinary literal parameter frozen at compile time.
type paramSpec struct {
	slot  *slotBase
	value any
}

// slotCompiler is the per-compilation state attached to a SQLBuilder while a statement is compiled.
// It is never stored globally and never mutates the serialized query tree.
type slotCompiler struct {
	reusePlaceholders bool

	// plan is the frozen, ordered list of parameter positions, interleaving ordinary literals
	// and slot occurrences exactly as they appear in the SQL text.
	plan []paramSpec

	// slotOrdinals maps each slot to the ordinal (1-based) allocated for its first occurrence.
	// The map also serves as the set of slots used by the compiled statement.
	slotOrdinals map[*slotBase]int

	// slotsInOrder keeps slots in first-occurrence order, for stable diagnostics.
	slotsInOrder []*slotBase
}

func newSlotCompiler(reusePlaceholders bool) *slotCompiler {
	return &slotCompiler{
		reusePlaceholders: reusePlaceholders,
		slotOrdinals:      map[*slotBase]int{},
	}
}

// addConstant freezes an ordinary literal parameter at the next parameter ordinal.
func (c *slotCompiler) addConstant(out *SQLBuilder, value any) {
	ordinal := len(c.plan) + 1
	c.plan = append(c.plan, paramSpec{value: copyParameterBytes(value)})
	out.WriteString(out.Dialect.ArgumentPlaceholder()(ordinal))
}

// addSlot registers a slot occurrence at the next parameter ordinal. In placeholder-reusing
// dialects, a repeated slot renders the already allocated placeholder and does not grow the plan;
// in positional '?' dialects every occurrence grows the plan and the argument is copied again at
// Bind time.
func (c *slotCompiler) addSlot(slot *slotBase, out *SQLBuilder) {
	if ordinal, ok := c.slotOrdinals[slot]; ok && c.reusePlaceholders {
		out.WriteString(out.Dialect.ArgumentPlaceholder()(ordinal))
		return
	}

	ordinal := len(c.plan) + 1
	c.plan = append(c.plan, paramSpec{slot: slot})

	if _, ok := c.slotOrdinals[slot]; !ok {
		c.slotOrdinals[slot] = ordinal
		c.slotsInOrder = append(c.slotsInOrder, slot)
	}

	out.WriteString(out.Dialect.ArgumentPlaceholder()(ordinal))
}

// copyParameterBytes returns a defensive copy of mutable []byte parameters so that a compiled
// statement and every Bind result own their argument bytes. Later binds, or caller mutations of
// the original slice, can never overwrite arguments already produced.
func copyParameterBytes(value any) any {
	if bytes, ok := value.([]byte); ok {
		copied := make([]byte, len(bytes))
		copy(copied, bytes)
		return copied
	}

	return value
}

// CompiledStatement is a statement compiled once into a fixed SQL shape and a fixed parameter
// plan. It is safe to call Bind concurrently from multiple goroutines: the compiled statement
// keeps no per-bind mutable state and every Bind returns an independent argument slice.
type CompiledStatement interface {
	// Sql returns the frozen parameterized SQL. The text and the placeholder positions never
	// change, regardless of how many times Bind is called.
	Sql() string

	// Slots returns the slots used by the statement in first-occurrence order. Slots sharing a
	// name remain distinct entries. The returned slice is a copy and can be retained by the caller.
	Slots() []Slot

	// Bind produces an executable Statement by resolving the frozen parameter plan against the
	// supplied bindings. Missing bindings, extra bindings, NULL on a non-nullable slot, and values
	// incompatible with the slot type are reported as errors before any database call. Bind never
	// traverses or mutates the original query tree and never touches previously bound arguments.
	Bind(bindings ...SlotBinding) (Statement, error)
}

type compiledStatement struct {
	query         string
	dialect       Dialect
	statementType StatementType

	plan         []paramSpec
	slotOrdinals map[*slotBase]int
	slots        []Slot
}

func (c *compiledStatement) Sql() string {
	return c.query
}

func (c *compiledStatement) Slots() []Slot {
	slots := make([]Slot, len(c.slots))
	copy(slots, c.slots)
	return slots
}

func (c *compiledStatement) Bind(bindings ...SlotBinding) (Statement, error) {
	values := make(map[*slotBase]any, len(bindings))

	for _, binding := range bindings {
		if binding.slot == nil {
			return nil, errors.New("jet: cannot bind a nil slot")
		}

		base := binding.slot.base()

		if _, exists := values[base]; exists {
			return nil, fmt.Errorf("jet: slot %q is bound more than once", base.name)
		}

		if err := validateSlotValue(base, binding.value); err != nil {
			return nil, err
		}

		values[base] = binding.value
	}

	// Every binding must reference a slot actually used by this compiled statement.
	for base := range values {
		if _, ok := c.slotOrdinals[base]; !ok {
			return nil, fmt.Errorf("jet: slot %q does not appear in the compiled statement", base.name)
		}
	}

	// Resolve the frozen plan into a brand-new argument slice.
	args := make([]any, len(c.plan))

	for i, spec := range c.plan {
		if spec.slot == nil {
			// Ordinary literal frozen at compile time; bytes were already copied during Compile.
			args[i] = spec.value
			continue
		}

		value, ok := values[spec.slot]
		if !ok {
			return nil, fmt.Errorf("jet: missing value for slot %q", spec.slot.name)
		}

		// Copy mutable bytes per Bind result so successive Binds never share writable storage.
		args[i] = copyParameterBytes(value)
	}

	return &boundStatement{
		dialect:       c.dialect,
		statementType: c.statementType,
		query:         c.query,
		args:          args,
	}, nil
}

func validateSlotValue(slot *slotBase, value any) error {
	if is.Nil(value) {
		if !slot.nullable {
			return fmt.Errorf("jet: cannot bind NULL to non-nullable slot %q", slot.name)
		}

		return nil
	}

	// Trust driver.Valuer implementations: database/sql drives their conversion at execution time.
	// Value() is intentionally not called here or during Compile/DebugSql compilation.
	if _, ok := value.(driver.Valuer); ok {
		return nil
	}

	if slot.kind.accepts(value) {
		return nil
	}

	return fmt.Errorf("jet: slot %q expects %s value, got %T", slot.name, slot.kind, value)
}

// boundStatement is an executable statement backed by frozen SQL and an independently owned
// argument slice. It implements Statement without holding any reference to the query tree.
type boundStatement struct {
	dialect       Dialect
	statementType StatementType
	query         string
	args          []any
}

func (b *boundStatement) Sql() (query string, args []interface{}) {
	return b.query, b.args
}

// Compile turns an already bound statement back into a slot-free compiled statement: every frozen
// parameter position is treated as an ordinary literal constant. Binding the result without slots
// reproduces an equivalent bound statement.
func (b *boundStatement) Compile() (CompiledStatement, error) {
	plan := make([]paramSpec, len(b.args))

	for i, arg := range b.args {
		plan[i] = paramSpec{value: copyParameterBytes(arg)}
	}

	return &compiledStatement{
		query:         b.query,
		dialect:       b.dialect,
		statementType: b.statementType,
		plan:          plan,
		slotOrdinals:  map[*slotBase]int{},
		slots:         []Slot{},
	}, nil
}

func (b *boundStatement) DebugSql() string {
	return renderDebugSql(b.dialect, b.query, b.args)
}

func (b *boundStatement) Query(db qrm.Queryable, destination interface{}) error {
	return b.QueryContext(context.Background(), db, destination)
}

func (b *boundStatement) QueryContext(ctx context.Context, db qrm.Queryable, destination interface{}) error {
	return statementQuery(ctx, db, destination, b.statementType, b, b.Sql)
}

func (b *boundStatement) Exec(db qrm.Executable) (sql.Result, error) {
	return b.ExecContext(context.Background(), db)
}

func (b *boundStatement) ExecContext(ctx context.Context, db qrm.Executable) (sql.Result, error) {
	return statementExec(ctx, db, b, b.Sql)
}

func (b *boundStatement) Rows(ctx context.Context, db qrm.Queryable) (*Rows, error) {
	return statementRows(ctx, db, b, b.Sql)
}

var numberedPlaceholderRegexp = regexp.MustCompile(`\$[0-9]+`)

// renderDebugSql inlines the bound arguments into the frozen SQL text using the dialect's
// existing debug rendering. It operates on the frozen string instead of re-serializing the query
// tree, so binding never requires a second tree traversal.
func renderDebugSql(dialect Dialect, query string, args []any) string {
	debugBuilder := &SQLBuilder{Dialect: dialect, Debug: true}

	if dialect.ReuseArgumentPlaceholders() {
		return numberedPlaceholderRegexp.ReplaceAllStringFunc(query, func(placeholder string) string {
			ordinal, err := strconv.Atoi(placeholder[1:])
			if err != nil || ordinal < 1 || ordinal > len(args) {
				return placeholder
			}

			return debugBuilder.argToString(args[ordinal-1])
		})
	}

	var result strings.Builder

	argIndex := 0

	for _, char := range query {
		if char == '?' && argIndex < len(args) {
			result.WriteString(debugBuilder.argToString(args[argIndex]))
			argIndex++
		} else {
			result.WriteRune(char)
		}
	}

	return result.String()
}
