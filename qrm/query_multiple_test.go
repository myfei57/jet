package qrm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Controlled scripted driver.
//
// It implements only database/sql/driver and driver.RowsNextResultSet, so the
// multiple result set behavior can be exercised deterministically: column
// layout switches, empty middle sets, errors while iterating or advancing,
// close tracking and context cancellation.
// ---------------------------------------------------------------------------

type nextGate struct {
	entered  chan struct{}
	release  chan struct{}
	signaled bool
	mu       sync.Mutex
}

func newNextGate() *nextGate {
	return &nextGate{entered: make(chan struct{}), release: make(chan struct{})}
}

// signalEntered closes entered exactly once.
func (g *nextGate) signalEntered() {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.signaled {
		g.signaled = true
		close(g.entered)
	}
}

type mockSet struct {
	columns      []string
	rows         [][]driver.Value
	iterationErr error // returned by Next instead of io.EOF after the rows are read
	advanceErr   error // returned by driver NextResultSet when advancing away from this set
	gate         *nextGate
}

type mockStats struct {
	mu                 sync.Mutex
	queries            int
	rowsCloses         int
	connectionsClosed  int
	nextResultSetCalls int
}

func (s *mockStats) snapshot() (queries, rowsCloses, connectionsClosed, nextResultSetCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.queries, s.rowsCloses, s.connectionsClosed, s.nextResultSetCalls
}

type mockDriver struct {
	sets  []mockSet
	stats *mockStats
}

var driverSequence int64

func newMockDB(t *testing.T, sets []mockSet) (*sql.DB, *mockStats) {
	t.Helper()

	stats := &mockStats{}
	name := fmt.Sprintf("jet-qrm-multi-%d", atomic.AddInt64(&driverSequence, 1))
	sql.Register(name, &mockDriver{sets: sets, stats: stats})

	db, err := sql.Open(name, "")
	require.NoError(t, err)

	t.Cleanup(func() { _ = db.Close() })

	return db, stats
}

func (d *mockDriver) Open(name string) (driver.Conn, error) {
	return &mockConn{sets: d.sets, stats: d.stats}, nil
}

type mockConn struct {
	sets  []mockSet
	stats *mockStats
}

func (c *mockConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("jet mock driver: Prepare is not supported")
}

func (c *mockConn) Close() error {
	c.stats.mu.Lock()
	c.stats.connectionsClosed++
	c.stats.mu.Unlock()

	return nil
}

func (c *mockConn) Begin() (driver.Tx, error) {
	return nil, errors.New("jet mock driver: Begin is not supported")
}

func (c *mockConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	c.stats.mu.Lock()
	c.stats.queries++
	c.stats.mu.Unlock()

	return &mockRows{sets: c.sets, stats: c.stats, ctx: ctx}, nil
}

type mockRows struct {
	sets  []mockSet
	stats *mockStats
	ctx   context.Context

	mu          sync.Mutex
	setIndex    int
	rowIndex    int
	closed      bool
	gateHandled bool
}

func (r *mockRows) Columns() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.sets[r.setIndex].columns
}

func (r *mockRows) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.closed {
		r.closed = true
		r.stats.mu.Lock()
		r.stats.rowsCloses++
		r.stats.mu.Unlock()
	}

	return nil
}

func (r *mockRows) Next(dest []driver.Value) error {
	r.mu.Lock()
	set := r.sets[r.setIndex]
	gate := set.gate
	useGate := gate != nil && r.rowIndex == 0 && !r.gateHandled
	if useGate {
		r.gateHandled = true
	}
	r.mu.Unlock()

	if useGate {
		gate.signalEntered()
		<-gate.release

		// Real drivers abort a blocked Next when the query context is canceled.
		if err := r.ctx.Err(); err != nil {
			return err
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	set = r.sets[r.setIndex]

	if r.rowIndex < len(set.rows) {
		copy(dest, set.rows[r.rowIndex])
		r.rowIndex++
		return nil
	}

	if set.iterationErr != nil {
		return set.iterationErr
	}

	return io.EOF
}

func (r *mockRows) HasNextResultSet() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	// A set with advanceErr models a driver that knows another result (or a
	// trailing error) is pending, so database/sql must keep the rows open.
	return r.setIndex < len(r.sets)-1 || r.sets[r.setIndex].advanceErr != nil
}

func (r *mockRows) NextResultSet() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.stats.mu.Lock()
	r.stats.nextResultSetCalls++
	r.stats.mu.Unlock()

	current := r.sets[r.setIndex]

	if current.advanceErr != nil {
		return current.advanceErr
	}

	r.setIndex++
	r.rowIndex = 0
	r.gateHandled = false

	return nil
}

// ---------------------------------------------------------------------------
// Destination model types used by the tests.
// ---------------------------------------------------------------------------

type MultiItem struct {
	ID   int64 `sql:"primary_key"`
	Name string
}

type MultiParent struct {
	ID   int64 `sql:"primary_key"`
	Name string
}

type MultiChild struct {
	ID    int64 `sql:"primary_key"`
	Title string
}

type MultiParentWithChildren struct {
	MultiParent
	Children []MultiChild
}

func int64V(v ...int64) []driver.Value {
	row := make([]driver.Value, len(v))
	for i := range v {
		row[i] = v[i]
	}
	return row
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestQueryMultipleHappyPath(t *testing.T) {
	db, stats := newMockDB(t, []mockSet{
		{
			columns: []string{"multiitem.id", "multiitem.name"},
			rows: [][]driver.Value{
				{int64(1), "a"},
				{int64(2), "b"},
			},
		},
		{
			columns: []string{"multiitem.name", "multiitem.id"}, // reordered columns, same struct type
			rows: [][]driver.Value{
				{"c", int64(3)},
			},
		},
	})

	var items1 []MultiItem
	var items2 []MultiItem

	processed, err := QueryMultiple(context.Background(), db, "QUERY", nil,
		ResultSet{DestPtr: &items1},
		ResultSet{DestPtr: &items2},
	)

	require.NoError(t, err)
	require.Equal(t, int64(3), processed)
	require.Equal(t, []MultiItem{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}}, items1)
	require.Equal(t, []MultiItem{{ID: 3, Name: "c"}}, items2)

	// SQL is executed exactly once and the rows are closed.
	queries, rowsCloses, _, advances := stats.snapshot()
	require.Equal(t, 1, queries)
	require.Equal(t, 1, rowsCloses)
	require.Equal(t, 1, advances)

	// Caller DB is not closed: it can be used again.
	var items3, items4 []MultiItem
	_, err = QueryMultiple(context.Background(), db, "QUERY", nil,
		ResultSet{DestPtr: &items3},
		ResultSet{DestPtr: &items4},
	)
	require.NoError(t, err)
	require.Equal(t, []MultiItem{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}}, items3)
	require.Equal(t, []MultiItem{{ID: 3, Name: "c"}}, items4)

	// Executing the query a second time re-runs the SQL, it does not reuse old rows.
	queries2, _, _, _ := stats.snapshot()
	require.Equal(t, 2, queries2)
}

func TestQueryMultipleNestedGroupingAndDedup(t *testing.T) {
	db, stats := newMockDB(t, []mockSet{
		{
			columns: []string{"multiparent.id", "multiparent.name", "multichild.id", "multichild.title"},
			rows: [][]driver.Value{
				{int64(10), "P1", int64(100), "c1"},
				{int64(10), "P1", int64(101), "c2"},
				{int64(11), "P2", int64(102), "c3"},
			},
		},
		{
			// Same struct type names, completely different column order.
			columns: []string{"multichild.id", "multichild.title", "multiparent.name", "multiparent.id"},
			rows: [][]driver.Value{
				{int64(200), "x", "P9", int64(20)},
			},
		},
	})

	var first []MultiParentWithChildren
	var second []MultiParentWithChildren

	_, err := QueryMultiple(context.Background(), db, "QUERY", nil,
		ResultSet{DestPtr: &first},
		ResultSet{DestPtr: &second},
	)

	require.NoError(t, err)

	require.Equal(t, []MultiParentWithChildren{
		{
			MultiParent: MultiParent{ID: 10, Name: "P1"},
			Children: []MultiChild{
				{ID: 100, Title: "c1"},
				{ID: 101, Title: "c2"},
			},
		},
		{
			MultiParent: MultiParent{ID: 11, Name: "P2"},
			Children:    []MultiChild{{ID: 102, Title: "c3"}},
		},
	}, first)

	require.Equal(t, []MultiParentWithChildren{
		{
			MultiParent: MultiParent{ID: 20, Name: "P9"},
			Children:    []MultiChild{{ID: 200, Title: "x"}},
		},
	}, second)

	_, rowsCloses, _, advances := stats.snapshot()
	require.Equal(t, 1, rowsCloses)
	require.Equal(t, 1, advances)
}

func TestQueryMultipleEmptyMiddleSetWithSliceIsLegal(t *testing.T) {
	db, stats := newMockDB(t, []mockSet{
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(1)}},
		{columns: []string{"id"}, rows: nil}, // empty middle result set
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(9)}},
	})

	var first []int64
	var middle []int64
	var last []int64

	processed, err := QueryMultiple(context.Background(), db, "QUERY", nil,
		ResultSet{DestPtr: &first},
		ResultSet{DestPtr: &middle},
		ResultSet{DestPtr: &last},
	)

	require.NoError(t, err)
	require.Equal(t, int64(2), processed)
	require.Equal(t, []int64{1}, first)
	require.Len(t, middle, 0, "empty middle result set is a legal empty slice")
	require.Equal(t, []int64{9}, last)

	// The empty middle set does not cause later data to be skipped: both
	// result set advances happened.
	_, _, _, advances := stats.snapshot()
	require.Equal(t, 2, advances)
}

func TestQueryMultipleEmptyStructSetReportsErrNoRowsWithPosition(t *testing.T) {
	db, stats := newMockDB(t, []mockSet{
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(1)}},
		{columns: []string{"multiitem.id", "multiitem.name"}, rows: nil}, // empty, struct destination
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(9)}},
	})

	first := []int64{-100}
	middle := MultiItem{ID: 777, Name: "sentinel"}
	last := []int64{-200}

	processed, err := QueryMultiple(context.Background(), db, "QUERY", nil,
		ResultSet{DestPtr: &first},
		ResultSet{DestPtr: &middle},
		ResultSet{DestPtr: &last},
	)

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoRows))
	require.EqualError(t, err, "jet: result set 2: qrm: no rows in result set")
	require.Equal(t, int64(2), processed)

	// Nothing is published to any caller destination, and later sets were still read.
	require.Equal(t, []int64{-100}, first)
	require.Equal(t, MultiItem{ID: 777, Name: "sentinel"}, middle)
	require.Equal(t, []int64{-200}, last)

	_, rowsCloses, _, advances := stats.snapshot()
	require.Equal(t, 1, rowsCloses)
	require.Equal(t, 2, advances)
}

func TestQueryMultipleStructDestHappyPath(t *testing.T) {
	db, _ := newMockDB(t, []mockSet{
		{
			columns: []string{"multiitem.id", "multiitem.name"},
			rows: [][]driver.Value{
				{int64(1), "first"},
				{int64(2), "second"},
			},
		},
	})

	var item MultiItem

	processed, err := QueryMultiple(context.Background(), db, "QUERY", nil, ResultSet{DestPtr: &item})

	require.NoError(t, err)
	require.Equal(t, int64(2), processed)
	require.Equal(t, MultiItem{ID: 1, Name: "first"}, item)
}

func TestQueryMultipleFewerResultSetsThanDestinations(t *testing.T) {
	db, stats := newMockDB(t, []mockSet{
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(1)}},
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(2)}},
	})

	first := []int64{-1}
	second := []int64{-2}
	third := []int64{-3}

	_, err := QueryMultiple(context.Background(), db, "QUERY", nil,
		ResultSet{DestPtr: &first},
		ResultSet{DestPtr: &second},
		ResultSet{DestPtr: &third},
	)

	require.Error(t, err)
	require.EqualError(t, err, "jet: query returned 2 result set(s), but 3 destinations were provided")

	// No partial publishing.
	require.Equal(t, []int64{-1}, first)
	require.Equal(t, []int64{-2}, second)
	require.Equal(t, []int64{-3}, third)

	_, rowsCloses, _, _ := stats.snapshot()
	require.Equal(t, 1, rowsCloses)
}

func TestQueryMultipleMoreResultSetsThanDestinations(t *testing.T) {
	db, stats := newMockDB(t, []mockSet{
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(1)}},
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(2)}},
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(3)}},
	})

	first := []int64{-1}
	second := []int64{-2}

	_, err := QueryMultiple(context.Background(), db, "QUERY", nil,
		ResultSet{DestPtr: &first},
		ResultSet{DestPtr: &second},
	)

	require.Error(t, err)
	require.EqualError(t, err, "jet: query returned more result sets than destinations: result set 3 has no destination")

	require.Equal(t, []int64{-1}, first)
	require.Equal(t, []int64{-2}, second)

	_, rowsCloses, _, _ := stats.snapshot()
	require.Equal(t, 1, rowsCloses)
}

func TestQueryMultipleDuplicateAliasAmbiguity(t *testing.T) {
	t.Run("first result set", func(t *testing.T) {
		db, stats := newMockDB(t, []mockSet{
			{columns: []string{"ID", "id"}, rows: [][]driver.Value{int64V(1, 2)}},
		})

		var items []MultiItem

		_, err := QueryMultiple(context.Background(), db, "QUERY", nil, ResultSet{DestPtr: &items})

		require.Error(t, err)
		require.Contains(t, err.Error(), "jet: result set 1:")
		require.Contains(t, err.Error(), "ambiguous duplicate column alias 'id'")

		_, rowsCloses, _, _ := stats.snapshot()
		require.Equal(t, 1, rowsCloses)
	})

	t.Run("later result set keeps previous destinations untouched", func(t *testing.T) {
		db, stats := newMockDB(t, []mockSet{
			{columns: []string{"id"}, rows: [][]driver.Value{int64V(1)}},
			{columns: []string{"Actor.ID", "actor.id"}, rows: [][]driver.Value{{int64(2), int64(3)}}},
		})

		first := []int64{-1}
		var second []MultiItem

		_, err := QueryMultiple(context.Background(), db, "QUERY", nil,
			ResultSet{DestPtr: &first},
			ResultSet{DestPtr: &second},
		)

		require.Error(t, err)
		require.Contains(t, err.Error(), "jet: result set 2:")
		require.Contains(t, err.Error(), "ambiguous duplicate column alias 'actor.id'")
		require.Equal(t, []int64{-1}, first)
		require.Nil(t, second)

		_, rowsCloses, _, advances := stats.snapshot()
		require.Equal(t, 1, rowsCloses)
		require.Equal(t, 1, advances)
	})
}

func TestQueryMultipleScanFailure(t *testing.T) {
	db, stats := newMockDB(t, []mockSet{
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(1)}},
		{columns: []string{"id"}, rows: [][]driver.Value{{"not-a-number"}}},
	})

	first := []int64{-1}
	var second []int64

	_, err := QueryMultiple(context.Background(), db, "QUERY", nil,
		ResultSet{DestPtr: &first},
		ResultSet{DestPtr: &second},
	)

	require.Error(t, err)
	require.Contains(t, err.Error(), "jet: result set 2:")
	require.Contains(t, err.Error(), "to []int64")
	require.Contains(t, err.Error(), "invalid syntax")

	// First result set is not published when a later result set fails.
	require.Equal(t, []int64{-1}, first)
	require.Nil(t, second)

	_, rowsCloses, _, advances := stats.snapshot()
	require.Equal(t, 1, rowsCloses)
	require.Equal(t, 1, advances)
}

func TestQueryMultipleIterationError(t *testing.T) {
	iterationErr := errors.New("boom: iteration failed")

	db, stats := newMockDB(t, []mockSet{
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(1)}},
		{columns: []string{"id"}, rows: nil, iterationErr: iterationErr},
	})

	first := []int64{-1}
	var second []int64

	_, err := QueryMultiple(context.Background(), db, "QUERY", nil,
		ResultSet{DestPtr: &first},
		ResultSet{DestPtr: &second},
	)

	require.Error(t, err)
	require.ErrorIs(t, err, iterationErr)
	require.EqualError(t, err, "jet: result set 2: boom: iteration failed")
	require.Equal(t, []int64{-1}, first)

	_, rowsCloses, _, _ := stats.snapshot()
	require.Equal(t, 1, rowsCloses)
}

func TestQueryMultipleAdvanceError(t *testing.T) {
	advanceErr := errors.New("boom: cannot advance")

	t.Run("between result sets", func(t *testing.T) {
		db, stats := newMockDB(t, []mockSet{
			{columns: []string{"id"}, rows: [][]driver.Value{int64V(1)}, advanceErr: advanceErr},
			{columns: []string{"id"}, rows: [][]driver.Value{int64V(2)}},
		})

		first := []int64{-1}
		second := []int64{-2}

		_, err := QueryMultiple(context.Background(), db, "QUERY", nil,
			ResultSet{DestPtr: &first},
			ResultSet{DestPtr: &second},
		)

		require.Error(t, err)
		require.ErrorIs(t, err, advanceErr)
		require.EqualError(t, err, "jet: result set 2: boom: cannot advance")
		require.Equal(t, []int64{-1}, first)
		require.Equal(t, []int64{-2}, second)

		_, rowsCloses, _, _ := stats.snapshot()
		require.Equal(t, 1, rowsCloses)
	})

	t.Run("after the last destination is reported through rows.Err", func(t *testing.T) {
		db, stats := newMockDB(t, []mockSet{
			{columns: []string{"id"}, rows: [][]driver.Value{int64V(1)}, advanceErr: advanceErr},
		})

		first := []int64{-1}

		_, err := QueryMultiple(context.Background(), db, "QUERY", nil, ResultSet{DestPtr: &first})

		require.Error(t, err)
		require.ErrorIs(t, err, advanceErr)
		require.Equal(t, []int64{-1}, first)

		_, rowsCloses, _, _ := stats.snapshot()
		require.Equal(t, 1, rowsCloses)
	})
}

func TestQueryMultipleCancellationStopsReading(t *testing.T) {
	gate := newNextGate()

	db, stats := newMockDB(t, []mockSet{
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(1)}},
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(2)}, gate: gate},
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(3)}},
	})

	ctx, cancel := context.WithCancel(context.Background())

	var first []int64
	var second []int64
	var third []int64

	done := make(chan error, 1)

	go func() {
		_, err := QueryMultiple(ctx, db, "QUERY", nil,
			ResultSet{DestPtr: &first},
			ResultSet{DestPtr: &second},
			ResultSet{DestPtr: &third},
		)
		done <- err
	}()

	<-gate.entered

	// Cancel while the second result set is being read, then let the driver Next return.
	cancel()
	close(gate.release)

	err := <-done

	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	// Nothing is published, and reading never advanced past the cancelled result set.
	require.Nil(t, first)
	require.Nil(t, second)
	require.Nil(t, third)

	_, rowsCloses, _, advances := stats.snapshot()
	require.Equal(t, 1, rowsCloses)
	require.Equal(t, 1, advances, "must not advance to result set 3 after cancellation")
}

func TestQueryMultiplePerResultSetConfig(t *testing.T) {
	db, _ := newMockDB(t, []mockSet{
		// Every column maps to a field: strict scan passes.
		{columns: []string{"multiitem.id", "multiitem.name"}, rows: [][]driver.Value{{int64(1), "a"}}},
		// One extra column is never read: non-strict config allows it.
		{columns: []string{"multiitem.id", "multiitem.name", "unused"}, rows: [][]driver.Value{{int64(2), "b", "x"}}},
	})

	var first []MultiItem
	var second []MultiItem

	_, err := QueryMultiple(context.Background(), db, "QUERY", nil,
		ResultSet{DestPtr: &first, Config: &Config{StrictScan: true}},
		ResultSet{DestPtr: &second, Config: &Config{StrictScan: false}},
	)

	require.NoError(t, err)
	require.Equal(t, []MultiItem{{ID: 1, Name: "a"}}, first)
	require.Equal(t, []MultiItem{{ID: 2, Name: "b"}}, second)
}

func TestQueryMultipleStrictPanicStillClosesRows(t *testing.T) {
	db, stats := newMockDB(t, []mockSet{
		{columns: []string{"multiitem.id", "multiitem.name", "unused"}, rows: [][]driver.Value{{int64(1), "a", "x"}}},
	})

	var items []MultiItem

	require.PanicsWithValue(t, "jet: columns never used: 'unused'", func() {
		_, _ = QueryMultiple(context.Background(), db, "QUERY", nil,
			ResultSet{DestPtr: &items, Config: &Config{StrictScan: true}},
		)
	})

	// Panic unwinds through the deferred rows.Close and no destination is published.
	require.Nil(t, items)
	_, rowsCloses, _, _ := stats.snapshot()
	require.Equal(t, 1, rowsCloses)
}

func TestQueryMultipleNilContextUsesBackground(t *testing.T) {
	db, _ := newMockDB(t, []mockSet{
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(1)}},
	})

	var ids []int64

	_, err := QueryMultiple(nil, db, "QUERY", nil, ResultSet{DestPtr: &ids})

	require.NoError(t, err)
	require.Equal(t, []int64{1}, ids)
}

func TestQueryMultipleInvalidArguments(t *testing.T) {
	db, _ := newMockDB(t, []mockSet{{columns: []string{"id"}}})

	t.Run("no destinations", func(t *testing.T) {
		require.PanicsWithValue(t, "jet: at least one result set destination is required", func() {
			_, _ = QueryMultiple(context.Background(), db, "QUERY", nil)
		})
	})

	t.Run("nil destination with position", func(t *testing.T) {
		var ids []int64
		require.PanicsWithValue(t, "jet: result set 2 destination is nil", func() {
			_, _ = QueryMultiple(context.Background(), db, "QUERY", nil,
				ResultSet{DestPtr: &ids},
				ResultSet{DestPtr: nil},
			)
		})
	})

	t.Run("non pointer destination", func(t *testing.T) {
		require.PanicsWithValue(t, "jet: destination has to be a pointer to slice or pointer to struct", func() {
			var ids []int64
			notAPtr := MultiItem{}
			_, _ = QueryMultiple(context.Background(), db, "QUERY", nil,
				ResultSet{DestPtr: &ids},
				ResultSet{DestPtr: notAPtr},
			)
		})
	})

	t.Run("pointer to neither slice nor struct", func(t *testing.T) {
		require.PanicsWithValue(t, "jet: destination has to be a pointer to slice or pointer to struct", func() {
			var ids []int64
			number := 1
			_, _ = QueryMultiple(context.Background(), db, "QUERY", nil,
				ResultSet{DestPtr: &ids},
				ResultSet{DestPtr: &number},
			)
		})
	})

	t.Run("nil db", func(t *testing.T) {
		require.PanicsWithValue(t, "jet: db is nil", func() {
			var ids []int64
			_, _ = QueryMultiple(context.Background(), nil, "QUERY", nil, ResultSet{DestPtr: &ids})
		})
	})
}

func TestQueryMultipleAtomicPublishOnLateError(t *testing.T) {
	advanceErr := errors.New("boom after last set")

	db, stats := newMockDB(t, []mockSet{
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(1)}},
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(2)}, advanceErr: advanceErr},
	})

	first := []int64{-1}
	second := []int64{-2}

	_, err := QueryMultiple(context.Background(), db, "QUERY", nil,
		ResultSet{DestPtr: &first},
		ResultSet{DestPtr: &second},
	)

	require.Error(t, err)
	require.ErrorIs(t, err, advanceErr)

	// Even though both result sets were fully mapped into temporary destinations,
	// the trailing error prevents any caller destination update.
	require.Equal(t, []int64{-1}, first)
	require.Equal(t, []int64{-2}, second)

	queries, rowsCloses, _, _ := stats.snapshot()
	require.Equal(t, 1, queries)
	require.Equal(t, 1, rowsCloses)
}

func TestQueryMultipleZeroColumnResultSet(t *testing.T) {
	db, _ := newMockDB(t, []mockSet{
		{columns: nil, rows: [][]driver.Value{{}}},
		{columns: []string{"id"}, rows: [][]driver.Value{int64V(7)}},
	})

	var empty []struct{}
	var ids []int64

	processed, err := QueryMultiple(context.Background(), db, "QUERY", nil,
		ResultSet{DestPtr: &empty},
		ResultSet{DestPtr: &ids},
	)

	require.NoError(t, err)
	require.Equal(t, int64(1), processed)
	require.Len(t, empty, 0)
	require.Equal(t, []int64{7}, ids)
}
