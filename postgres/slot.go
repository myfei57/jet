package postgres

import "github.com/go-jet/jet/v2/internal/jet"

// Slot is a reusable, typed parameter placeholder. A slot is constructed once, embedded any number
// of times into a query (including CTEs and subqueries), and receives a value only when a compiled
// statement is bound.
type Slot = jet.Slot

// Typed slot interfaces.
type (
	// IntegerSlot is a slot accepting integer values.
	IntegerSlot = jet.IntegerSlot
	// FloatSlot is a slot accepting float values.
	FloatSlot = jet.FloatSlot
	// BoolSlot is a slot accepting boolean values.
	BoolSlot = jet.BoolSlot
	// StringSlot is a slot accepting string values.
	StringSlot = jet.StringSlot
	// DateSlot is a slot accepting date values.
	DateSlot = jet.DateSlot
	// TimeSlot is a slot accepting time values.
	TimeSlot = jet.TimeSlot
	// TimezSlot is a slot accepting time with time zone values.
	TimezSlot = jet.TimezSlot
	// TimestampSlot is a slot accepting timestamp values.
	TimestampSlot = jet.TimestampSlot
	// TimestampzSlot is a slot accepting timestamp with time zone values.
	TimestampzSlot = jet.TimestampzSlot
	// BlobSlot is a slot accepting binary ([]byte) values.
	BlobSlot = jet.BlobSlot
)

// SlotBinding pairs a slot with the value to bind to it.
type SlotBinding = jet.SlotBinding

// CompiledStatement is a statement compiled once into a fixed SQL shape and a fixed parameter plan.
// It is safe to call Bind concurrently.
type CompiledStatement = jet.CompiledStatement

// Slot constructors. NewNullable* variants explicitly allow binding SQL NULL.
var (
	NewIntSlot         = jet.NewIntSlot
	NewNullableIntSlot = jet.NewNullableIntSlot

	NewFloatSlot         = jet.NewFloatSlot
	NewNullableFloatSlot = jet.NewNullableFloatSlot

	NewBoolSlot         = jet.NewBoolSlot
	NewNullableBoolSlot = jet.NewNullableBoolSlot

	NewStringSlot         = jet.NewStringSlot
	NewNullableStringSlot = jet.NewNullableStringSlot

	NewDateSlot         = jet.NewDateSlot
	NewNullableDateSlot = jet.NewNullableDateSlot

	NewTimeSlot         = jet.NewTimeSlot
	NewNullableTimeSlot = jet.NewNullableTimeSlot

	NewTimezSlot         = jet.NewTimezSlot
	NewNullableTimezSlot = jet.NewNullableTimezSlot

	NewTimestampSlot         = jet.NewTimestampSlot
	NewNullableTimestampSlot = jet.NewNullableTimestampSlot

	NewTimestampzSlot         = jet.NewTimestampzSlot
	NewNullableTimestampzSlot = jet.NewNullableTimestampzSlot

	NewBlobSlot         = jet.NewBlobSlot
	NewNullableBlobSlot = jet.NewNullableBlobSlot

	// SlotValue creates a SlotBinding for slot. Pass nil to bind SQL NULL to a nullable slot.
	SlotValue = jet.SlotValue
)
