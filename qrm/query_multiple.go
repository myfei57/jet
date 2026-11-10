package qrm

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"

	"github.com/go-jet/jet/v2/internal/utils/must"
)

// ResultSet binds a destination pointer (and optional mapping configuration) to one
// result set of a query that returns multiple result sets (see [QueryMultiple]).
//
// ResultSet entries are matched to the result sets returned by the query by position:
// the first entry is mapped from the first result set, the second entry from the
// second result set, and so on.
type ResultSet struct {
	// DestPtr is the destination of one result set. It has to be a pointer to a struct
	// or a pointer to a slice, exactly like the destination of [Query].
	//
	// Different result sets can have completely different column layouts and map to
	// different nested struct or slice destinations.
	DestPtr interface{}

	// Config, when not nil, replaces [GlobalConfig] while mapping this result set.
	// This allows each result set to use its own strict scan / strict field mapping
	// configuration. When nil, [GlobalConfig] is used.
	Config *Config
}

// QueryMultiple executes a SQL query that returns multiple result sets and maps every
// result set into its own destination, using [database/sql.Rows.NextResultSet] to advance
// through the result sets returned by a single QueryContext call.
//
// The resultSets destinations are matched to result sets by position. Each destination
// must be a pointer to a struct or a pointer to a slice:
//
//   - A slice destination is valid for an empty result set, in which case it stays empty.
//   - A struct destination requires exactly one or more rows; an empty result set is
//     reported as [ErrNoRows] wrapped with the result set position (errors.Is unwraps
//     [ErrNoRows]). An empty result set never stops the remaining result sets from being
//     read.
//
// A fresh scan context is created for every result set: column names and indexes, primary
// key grouping, destination type information and strict scan state are rebuilt for each
// result set, so alias indexes and the nested object de-duplication cache of one result set
// are never reused for another. The same struct type used in two result sets with different
// column orders is mapped correctly.
//
// The query is executed exactly once. The SQL is not re-executed for any result set, and
// the same [database/sql.Rows] is never accessed concurrently. Result set count mismatches
// (fewer or more result sets than destinations), ambiguous duplicate column aliases, scan
// failures and the final [database/sql.Rows.Err] are all reported as errors identifying the
// result set position.
//
// Atomicity: all result sets are first mapped into temporary destinations. The caller
// destinations are updated only after every result set has been mapped successfully, so an
// error never leaves the first few destinations updated and the rest untouched.
//
// QueryMultiple closes the returned rows on every exit path, but it never closes the
// caller database connection or transaction.
func QueryMultiple(
	ctx context.Context,
	db Queryable,
	query string,
	args []interface{},
	resultSets ...ResultSet,
) (rowsProcessed int64, err error) {

	must.BeInitializedPtr(db, "jet: db is nil")
	must.BeTrue(len(resultSets) > 0, "jet: at least one result set destination is required")

	if ctx == nil {
		ctx = context.Background()
	}

	tempDestinations := make([]tempDestination, len(resultSets))

	for i := range resultSets {
		tempDestinations[i] = newTempDestination(resultSets[i].DestPtr, i)
	}

	var rows *sql.Rows

	rows, err = db.QueryContext(ctx, query, args...)

	if err != nil {
		return 0, err
	}

	defer rows.Close()

	// noRowsErr remembers the first empty struct result set. It is a "soft" error:
	// reading has to continue so that later result sets are not skipped. Nothing is
	// published to the caller destinations and the error is returned at the end.
	var noRowsErr error

	for i := range resultSets {
		config := GlobalConfig

		if resultSets[i].Config != nil {
			config = *resultSets[i].Config
		}

		// Each result set gets its own scan context: column layout, primary key group
		// keys, type info and strict scan state must never be carried over.
		var scanContext *ScanContext

		scanContext, err = newScanContext(rows, config, true)

		if err != nil {
			return rowsProcessed, wrapResultSetError(i, err)
		}

		var processed int64

		processed, err = scanRowsToSlice(rows, scanContext, tempDestinations[i].slicePtr)
		rowsProcessed += processed

		if err != nil {
			return rowsProcessed, wrapResultSetError(i, err)
		}

		if tempDestinations[i].isStructDest && processed == 0 && noRowsErr == nil {
			noRowsErr = wrapResultSetError(i, ErrNoRows)
		}

		if i < len(resultSets)-1 {
			// Advance to the result set expected by the next destination. An empty
			// current result set does not skip this step, so later data is never lost.
			if !rows.NextResultSet() {
				if rowsErr := rows.Err(); rowsErr != nil {
					return rowsProcessed, wrapResultSetError(i+1, rowsErr)
				}

				return rowsProcessed, fmt.Errorf(
					"jet: query returned %d result set(s), but %d destinations were provided",
					i+1, len(resultSets))
			}
		} else {
			// All destinations consumed: there must not be another result set.
			if rows.NextResultSet() {
				return rowsProcessed, fmt.Errorf(
					"jet: query returned more result sets than destinations: result set %d has no destination",
					len(resultSets)+1)
			}

			if rowsErr := rows.Err(); rowsErr != nil {
				return rowsProcessed, wrapResultSetError(i, rowsErr)
			}
		}
	}

	if noRowsErr != nil {
		return rowsProcessed, noRowsErr
	}

	// Every result set was mapped successfully: publish the temporary destinations
	// to the caller destinations in one step.
	for i := range tempDestinations {
		err = tempDestinations[i].publish()

		if err != nil {
			return rowsProcessed, wrapResultSetError(i, err)
		}
	}

	return rowsProcessed, nil
}

// wrapResultSetError annotates err with the 1-based result set position.
func wrapResultSetError(resultSetIndex int, err error) error {
	return fmt.Errorf("jet: result set %d: %w", resultSetIndex+1, err)
}

// tempDestination is the private scratch space for one result set. Caller destinations
// are touched only in publish, after every result set has been mapped successfully.
type tempDestination struct {
	// userPtr is the caller destination pointer: *T for a struct destination or *[]T
	// for a slice destination.
	userPtr reflect.Value

	// slicePtr is the temporary slice destination actually used for mapping:
	// *[]*T for a struct destination and *[]T for a slice destination.
	slicePtr reflect.Value

	isStructDest bool
}

func newTempDestination(destPtr interface{}, position int) tempDestination {
	must.BeInitializedPtr(destPtr,
		fmt.Sprintf("jet: result set %d destination is nil", position+1))
	must.BeTypeKind(destPtr, reflect.Ptr,
		"jet: destination has to be a pointer to slice or pointer to struct")

	userPtr := reflect.ValueOf(destPtr)

	switch userPtr.Type().Elem().Kind() {
	case reflect.Slice:
		// Fresh slice of the same element type, starting empty.
		slicePtr := reflect.New(userPtr.Type().Elem())

		return tempDestination{
			userPtr:      userPtr,
			slicePtr:     slicePtr,
			isStructDest: false,
		}
	case reflect.Struct:
		// Same wrapping trick as Query: map into a temporary slice of struct pointers.
		slicePtr := reflect.New(reflect.SliceOf(userPtr.Type()))

		return tempDestination{
			userPtr:      userPtr,
			slicePtr:     slicePtr,
			isStructDest: true,
		}
	default:
		panic("jet: destination has to be a pointer to slice or pointer to struct")
	}
}

// publish assigns the mapped temporary result to the caller destination.
// It is called only after all result sets were mapped without errors.
func (t tempDestination) publish() error {
	tempSliceValue := t.slicePtr.Elem()

	if !t.isStructDest {
		t.userPtr.Elem().Set(tempSliceValue)
		return nil
	}

	// Edge case from Query: rows contained only NULLs, nothing was appended.
	if tempSliceValue.Len() == 0 {
		return nil
	}

	structValue := t.userPtr.Elem()
	firstTempStruct := tempSliceValue.Index(0).Elem()

	if structValue.Type().AssignableTo(firstTempStruct.Type()) {
		structValue.Set(firstTempStruct)
	}

	return nil
}
