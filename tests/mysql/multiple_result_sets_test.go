package mysql

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/go-jet/jet/v2/qrm"
	"github.com/go-jet/jet/v2/tests/dbconfig"
	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
)

// Multiple result sets are enabled in the go-sql-driver by the multiStatements DSN
// parameter. A separate raw *sql.DB is opened so the shared package db (and its
// statement cache) is not involved or closed.
func openMultipleResultSetsDB(t *testing.T) *sql.DB {
	t.Helper()

	dsn := dbconfig.MySQLConnectionString(sourceIsMariaDB(), "") + "?multiStatements=true"

	rawDB, err := sql.Open("mysql", dsn)
	require.NoError(t, err)

	require.NoError(t, rawDB.Ping())

	t.Cleanup(func() { _ = rawDB.Close() })

	return rawDB
}

// queryCountingQueryable counts QueryContext calls, to prove the SQL of a multiple
// result set query is executed exactly once.
type queryCountingQueryable struct {
	qrm.Queryable
	queryCalls int
}

func (q *queryCountingQueryable) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	q.queryCalls++
	return q.Queryable.QueryContext(ctx, query, args...)
}

type realActor struct {
	ActorID   int32  `sql:"primary_key" alias:"actor.actor_id"`
	FirstName string `alias:"actor.first_name"`
	LastName  string `alias:"actor.last_name"`
}

type realActorWithFilms struct {
	realActor
	Films []realFilm `alias:"film"`
}

type realFilm struct {
	FilmID int32  `sql:"primary_key" alias:"film.film_id"`
	Title  string `alias:"film.title"`
}

const twoResultSetsSQL = `
SELECT actor.actor_id AS "actor.actor_id",
       actor.first_name AS "actor.first_name",
       actor.last_name AS "actor.last_name"
FROM dvds.actor
WHERE actor.actor_id IN (1, 2)
ORDER BY actor.actor_id;

SELECT actor.actor_id AS "actor.actor_id",
       actor.first_name AS "actor.first_name",
       film.film_id AS "film.film_id",
       film.title AS "film.title"
FROM dvds.actor
INNER JOIN dvds.film_actor ON film_actor.actor_id = actor.actor_id
INNER JOIN dvds.film ON film.film_id = film_actor.film_id
WHERE actor.actor_id = 1
ORDER BY film.film_id
LIMIT 3;
`

func TestQueryMultipleRealDatabase(t *testing.T) {
	rawDB := openMultipleResultSetsDB(t)
	counter := &queryCountingQueryable{Queryable: rawDB}

	var actors []realActor
	var oneActor realActor
	var actorFilms []realActorWithFilms

	threeSetsSQL := twoResultSetsSQL + `
SELECT actor.actor_id AS "actor.actor_id",
       actor.first_name AS "actor.first_name",
       actor.last_name AS "actor.last_name"
FROM dvds.actor
WHERE actor.actor_id = 2;
`

	processed, err := qrm.QueryMultiple(context.Background(), counter, threeSetsSQL, nil,
		qrm.ResultSet{DestPtr: &actors},
		qrm.ResultSet{DestPtr: &actorFilms},
		qrm.ResultSet{DestPtr: &oneActor},
	)

	require.NoError(t, err)

	// Set 1: different column layouts are mapped by position into each destination.
	require.Equal(t, []realActor{
		{ActorID: 1, FirstName: "PENELOPE", LastName: "GUINESS"},
		{ActorID: 2, FirstName: "NICK", LastName: "WAHLBERG"},
	}, actors)

	// Set 2: repeated join rows are de-duplicated into one actor with a nested slice.
	require.Len(t, actorFilms, 1)
	require.Equal(t, int32(1), actorFilms[0].ActorID)
	require.Equal(t, "PENELOPE", actorFilms[0].FirstName)
	require.Len(t, actorFilms[0].Films, 3)
	require.Equal(t, int32(1), actorFilms[0].Films[0].FilmID)
	require.NotEmpty(t, actorFilms[0].Films[0].Title)

	// Set 3: struct destination receives the single row.
	require.Equal(t, realActor{ActorID: 2, FirstName: "NICK", LastName: "WAHLBERG"}, oneActor)

	// 2 rows + 3 rows + 1 row were processed in total.
	require.Equal(t, int64(6), processed)

	// The SQL was executed exactly once, not once per result set.
	require.Equal(t, 1, counter.queryCalls)

	// The caller DB is still usable after QueryMultiple closed only the rows.
	require.NoError(t, rawDB.Ping())

	var again []realActor
	_, err = qrm.QueryMultiple(context.Background(), rawDB,
		`SELECT actor.actor_id AS "actor.actor_id", actor.first_name AS "actor.first_name",
		        actor.last_name AS "actor.last_name"
		 FROM dvds.actor WHERE actor.actor_id = 1;`,
		nil, qrm.ResultSet{DestPtr: &again})
	require.NoError(t, err)
	require.Len(t, again, 1)
}

func TestQueryMultipleRealDatabaseEmptyMiddleSetAndErrNoRows(t *testing.T) {
	rawDB := openMultipleResultSetsDB(t)
	counter := &queryCountingQueryable{Queryable: rawDB}

	sqlWithEmptyMiddleSet := `
SELECT actor.actor_id AS "actor.actor_id", actor.first_name AS "actor.first_name",
       actor.last_name AS "actor.last_name"
FROM dvds.actor WHERE actor.actor_id = 1;

SELECT actor.actor_id AS "actor.actor_id", actor.first_name AS "actor.first_name",
       actor.last_name AS "actor.last_name"
FROM dvds.actor WHERE actor.actor_id = -999;

SELECT actor.actor_id AS "actor.actor_id", actor.first_name AS "actor.first_name",
       actor.last_name AS "actor.last_name"
FROM dvds.actor WHERE actor.actor_id = 2;
`

	first := []realActor{{ActorID: -100, FirstName: "sentinel"}}
	var emptyStruct realActor
	last := []realActor{{ActorID: -200, FirstName: "sentinel"}}

	processed, err := qrm.QueryMultiple(context.Background(), counter, sqlWithEmptyMiddleSet, nil,
		qrm.ResultSet{DestPtr: &first},
		qrm.ResultSet{DestPtr: &emptyStruct},
		qrm.ResultSet{DestPtr: &last},
	)

	require.Error(t, err)
	require.True(t, errors.Is(err, qrm.ErrNoRows))
	require.EqualError(t, err, "jet: result set 2: qrm: no rows in result set")
	// The empty middle set did not prevent the third result set from being consumed.
	require.Equal(t, int64(2), processed)
	require.Equal(t, 1, counter.queryCalls)

	// No destination was updated.
	require.Equal(t, []realActor{{ActorID: -100, FirstName: "sentinel"}}, first)
	require.Equal(t, realActor{}, emptyStruct)
	require.Equal(t, []realActor{{ActorID: -200, FirstName: "sentinel"}}, last)
}

func TestQueryMultipleRealDatabaseScanFailureIsAtomic(t *testing.T) {
	rawDB := openMultipleResultSetsDB(t)
	counter := &queryCountingQueryable{Queryable: rawDB}

	type incompatible struct {
		FilmID bool `alias:"film.film_id"`
		Title  string
	}

	first := []realActor{{ActorID: -100, FirstName: "sentinel"}}
	var broken []incompatible

	_, err := qrm.QueryMultiple(context.Background(), counter, twoResultSetsSQL, nil,
		qrm.ResultSet{DestPtr: &first},
		qrm.ResultSet{DestPtr: &broken},
	)

	require.Error(t, err)
	require.Contains(t, err.Error(), "jet: result set 2:")
	require.Equal(t, 1, counter.queryCalls)

	// The successfully mapped first result set stays in its temporary destination.
	require.Equal(t, []realActor{{ActorID: -100, FirstName: "sentinel"}}, first)
	require.Nil(t, broken)
}

func TestQueryMultipleRealDatabaseExtraResultSet(t *testing.T) {
	rawDB := openMultipleResultSetsDB(t)
	counter := &queryCountingQueryable{Queryable: rawDB}

	var actors []realActor
	var actorFilms []realActorWithFilms
	var extra []realActor

	threeSetsSQL := twoResultSetsSQL + `
SELECT actor.actor_id AS "actor.actor_id", actor.first_name AS "actor.first_name",
       actor.last_name AS "actor.last_name"
FROM dvds.actor WHERE actor.actor_id = 1;
`

	_, err := qrm.QueryMultiple(context.Background(), counter, threeSetsSQL, nil,
		qrm.ResultSet{DestPtr: &actors},
		qrm.ResultSet{DestPtr: &actorFilms},
		qrm.ResultSet{DestPtr: &extra},
	)

	// Three destinations for three result sets succeeds; removing one destination
	// surfaces the trailing extra result set as an error (checked right below).
	require.NoError(t, err)
	require.Len(t, extra, 1)
	require.Equal(t, 1, counter.queryCalls)

	actors = nil
	actorFilms = nil
	counter.queryCalls = 0

	_, err = qrm.QueryMultiple(context.Background(), counter, threeSetsSQL, nil,
		qrm.ResultSet{DestPtr: &actors},
		qrm.ResultSet{DestPtr: &actorFilms},
	)

	require.EqualError(t, err, "jet: query returned more result sets than destinations: result set 3 has no destination")
	require.Nil(t, actors)
	require.Nil(t, actorFilms)
	require.Equal(t, 1, counter.queryCalls)
}
