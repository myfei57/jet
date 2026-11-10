package mysql

import (
	"database/sql/driver"
	"strings"
	"sync"
	"testing"

	"github.com/go-jet/jet/v2/internal/testutils"
	. "github.com/go-jet/jet/v2/mysql"
	"github.com/go-jet/jet/v2/qrm"
	"github.com/go-jet/jet/v2/tests/.gentestdata/mysql/dvds/model"
	. "github.com/go-jet/jet/v2/tests/.gentestdata/mysql/dvds/table"
	"github.com/stretchr/testify/require"
)

// literalActor is the equivalent query written with ordinary literals, used as the expected
// result baseline for every slot-based execution.
func literalActor(t *testing.T, id int64) model.Actor {
	t.Helper()

	var expected model.Actor
	require.NoError(t, SELECT(Actor.AllColumns).FROM(Actor).WHERE(Actor.ActorID.EQ(Int(id))).Query(db, &expected))

	return expected
}

func TestSlot_CompileOnceBindAndExecuteManyTimes(t *testing.T) {
	actorID := NewIntSlot("actor_id")

	statement := SELECT(Actor.AllColumns).FROM(Actor).WHERE(Actor.ActorID.EQ(actorID))

	compiled, err := statement.Compile()
	require.NoError(t, err)
	require.Len(t, compiled.Slots(), 1)
	require.Equal(t, "actor_id", compiled.Slots()[0].SlotName())

	frozenSQL := compiled.Sql()

	for _, id := range []int64{1, 2, 3} {
		bound, err := compiled.Bind(SlotValue(actorID, id))
		require.NoError(t, err)

		// The SQL shape is frozen and identical across binds.
		query, args := bound.Sql()
		require.Equal(t, frozenSQL, query)
		require.Equal(t, []any{id}, args)

		var actual model.Actor
		require.NoError(t, bound.Query(db, &actual))
		testutils.AssertDeepEqual(t, actual, literalActor(t, id))
	}
}

func TestSlot_RepeatedSlotCopiesQuestionArguments(t *testing.T) {
	actorID := NewIntSlot("actor_id")

	statement := SELECT(Actor.AllColumns).FROM(Actor).WHERE(
		Actor.ActorID.EQ(actorID).OR(Actor.ActorID.EQ(actorID)),
	)

	compiled, err := statement.Compile()
	require.NoError(t, err)

	// Every occurrence of the slot gets its own '?' placeholder.
	require.Equal(t, 2, strings.Count(compiled.Sql(), "?"))

	bound, err := compiled.Bind(SlotValue(actorID, int64(2)))
	require.NoError(t, err)

	_, args := bound.Sql()
	require.Equal(t, []any{int64(2), int64(2)}, args)

	var actual model.Actor
	require.NoError(t, bound.Query(db, &actual))
	testutils.AssertDeepEqual(t, actual, literalActor(t, 2))
}

func TestSlot_SameNameDifferentSlotsStayIndependent(t *testing.T) {
	first := NewIntSlot("same")
	second := NewIntSlot("same")
	require.NotSame(t, first, second)

	statement := SELECT(Actor.AllColumns).FROM(Actor).WHERE(
		Actor.ActorID.EQ(first).AND(Actor.ActorID.GT(second)),
	)

	compiled, err := statement.Compile()
	require.NoError(t, err)
	require.Len(t, compiled.Slots(), 2)

	bound, err := compiled.Bind(
		SlotValue(first, int64(2)),
		SlotValue(second, int64(1)),
	)
	require.NoError(t, err)

	_, args := bound.Sql()
	require.Equal(t, []any{int64(2), int64(1)}, args)

	var actual model.Actor
	require.NoError(t, bound.Query(db, &actual))
	testutils.AssertDeepEqual(t, actual, literalActor(t, 2))
}

func TestSlot_DeepWithBindsSharedSlotOnce(t *testing.T) {
	actorID := NewIntSlot("actor_id")

	filtered := CTE("filtered_actors")

	statement := WITH(filtered.AS(
		SELECT(Actor.AllColumns).FROM(Actor).WHERE(Actor.ActorID.GT(actorID)),
	))(
		SELECT(Actor.AllColumns).FROM(Actor).WHERE(Actor.ActorID.EQ(actorID)),
	)

	compiled, err := statement.Compile()
	require.NoError(t, err)

	// The slot is used once in the CTE and once in the outer query; each occurrence copies the arg.
	require.Equal(t, 2, strings.Count(compiled.Sql(), "?"))
	require.Len(t, compiled.Slots(), 1)

	bound, err := compiled.Bind(SlotValue(actorID, int64(2)))
	require.NoError(t, err)

	_, args := bound.Sql()
	require.Equal(t, []any{int64(2), int64(2)}, args)

	var actual model.Actor
	require.NoError(t, bound.Query(db, &actual))
	testutils.AssertDeepEqual(t, actual, literalActor(t, 2))
}

func TestSlot_NullableSlotAcceptsNullAndValue(t *testing.T) {
	firstName := NewNullableStringSlot("first_name")

	statement := SELECT(Actor.AllColumns).FROM(Actor).WHERE(
		Actor.FirstName.IS_NOT_DISTINCT_FROM(firstName),
	)

	compiled, err := statement.Compile()
	require.NoError(t, err)

	bound, err := compiled.Bind(SlotValue(firstName, "Penelope"))
	require.NoError(t, err)

	var actors []model.Actor
	require.NoError(t, bound.Query(db, &actors))
	require.NotEmpty(t, actors)
	for _, actor := range actors {
		require.Equal(t, "Penelope", actor.FirstName)
	}

	// NULL is allowed because the slot is explicitly nullable.
	nullBound, err := compiled.Bind(SlotValue(firstName, nil))
	require.NoError(t, err)
	require.Contains(t, nullBound.DebugSql(), "NULL")

	var nullActors []model.Actor
	require.NoError(t, nullBound.Query(db, &nullActors))
	require.Empty(t, nullActors)

	// A non-nullable slot rejects NULL before touching the database.
	nonNullable := NewStringSlot("required_name")
	rejectCompile, err := SELECT(Actor.ActorID).FROM(Actor).WHERE(
		Actor.FirstName.IS_NOT_DISTINCT_FROM(nonNullable),
	).Compile()
	require.NoError(t, err)

	_, err = rejectCompile.Bind(SlotValue(nonNullable, nil))
	require.EqualError(t, err, `jet: cannot bind NULL to non-nullable slot "required_name"`)
}

func TestSlot_BindingErrorsAreReportedBeforeExecution(t *testing.T) {
	actorID := NewIntSlot("actor_id")
	firstName := NewStringSlot("first_name")

	compiled, err := SELECT(Actor.AllColumns).FROM(Actor).WHERE(
		Actor.ActorID.EQ(actorID).AND(Actor.FirstName.EQ(firstName)),
	).Compile()
	require.NoError(t, err)

	_, err = compiled.Bind(SlotValue(actorID, int64(1)))
	require.EqualError(t, err, `jet: missing value for slot "first_name"`)

	_, err = compiled.Bind(
		SlotValue(actorID, int64(1)),
		SlotValue(firstName, "Penelope"),
		SlotValue(NewIntSlot("unused"), int64(3)),
	)
	require.EqualError(t, err, `jet: slot "unused" does not appear in the compiled statement`)

	_, err = compiled.Bind(
		SlotValue(actorID, "not-an-integer"),
		SlotValue(firstName, "Penelope"),
	)
	require.ErrorContains(t, err, `slot "actor_id" expects integer value, got string`)

	_, err = compiled.Bind(
		SlotValue(actorID, int64(1)),
		SlotValue(actorID, int64(2)),
		SlotValue(firstName, "Penelope"),
	)
	require.EqualError(t, err, `jet: slot "actor_id" is bound more than once`)
}

func TestSlot_ConcurrentBindAndQuery(t *testing.T) {
	actorID := NewIntSlot("actor_id")

	compiled, err := SELECT(Actor.AllColumns).FROM(Actor).WHERE(Actor.ActorID.EQ(actorID)).Compile()
	require.NoError(t, err)

	const goroutineCount = 30

	var waitGroup sync.WaitGroup
	waitGroup.Add(goroutineCount)

	for i := int64(1); i <= goroutineCount; i++ {
		id := i

		go func() {
			defer waitGroup.Done()

			bound, err := compiled.Bind(SlotValue(actorID, id))
			require.NoError(t, err)

			var actor model.Actor
			require.NoError(t, bound.Query(db, &actor))
			require.Equal(t, id, actor.ActorID)
		}()
	}

	waitGroup.Wait()
}

func TestSlot_ValuerIsNotEvaluatedAtCompileOrBind(t *testing.T) {
	actorID := NewIntSlot("actor_id")

	compiled, err := SELECT(Actor.AllColumns).FROM(Actor).WHERE(Actor.ActorID.EQ(actorID)).Compile()
	require.NoError(t, err)

	valuer := &slotRecordingValuer{value: int64(2)}

	bound, err := compiled.Bind(SlotValue(actorID, valuer))
	require.NoError(t, err)
	require.Zero(t, valuer.callCount(), "Compile and Bind must not call driver.Valuer.Value")

	var actor model.Actor
	require.NoError(t, bound.Query(db, &actor))
	require.GreaterOrEqual(t, valuer.callCount(), 1, "the database driver evaluates the Valuer at execution time")
	testutils.AssertDeepEqual(t, actor, literalActor(t, 2))
}

func TestSlot_RawNamedPlaceholdersKeepWorkingAlongsideSlots(t *testing.T) {
	actorID := NewIntSlot("actor_id")

	statement := SELECT(Actor.AllColumns).FROM(Actor).WHERE(
		Actor.ActorID.EQ(actorID).AND(RawBool("actor.first_name = :first_name", RawArgs{":first_name": "Nick"})),
	)

	compiled, err := statement.Compile()
	require.NoError(t, err)

	bound, err := compiled.Bind(SlotValue(actorID, int64(2)))
	require.NoError(t, err)

	query, args := bound.Sql()
	require.Equal(t, 2, strings.Count(query, "?"))
	require.Equal(t, []any{int64(2), "Nick"}, args)

	var actor model.Actor
	require.NoError(t, bound.Query(db, &actor))
	testutils.AssertDeepEqual(t, actor, literalActor(t, 2))
}

func TestSlot_BoundDebugSqlRendersValues(t *testing.T) {
	actorID := NewIntSlot("actor_id")

	compiled, err := SELECT(Actor.AllColumns).FROM(Actor).WHERE(Actor.ActorID.EQ(actorID)).Compile()
	require.NoError(t, err)

	bound, err := compiled.Bind(SlotValue(actorID, int64(2)))
	require.NoError(t, err)

	require.Contains(t, bound.DebugSql(), "actor.actor_id = 2")

	// qrm.ErrNoRows is part of the contract when no row matches.
	empty, err := compiled.Bind(SlotValue(actorID, int64(-1)))
	require.NoError(t, err)

	var missing model.Actor
	require.ErrorIs(t, empty.Query(db, &missing), qrm.ErrNoRows)
}

type slotRecordingValuer struct {
	mu    sync.Mutex
	calls int
	value driver.Value
}

func (v *slotRecordingValuer) Value() (driver.Value, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.calls++
	return v.value, nil
}

func (v *slotRecordingValuer) callCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.calls
}
