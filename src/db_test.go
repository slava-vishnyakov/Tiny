package main

import (
	"os"
	"strings"
	"testing"
)

func assertTinyOutput(t *testing.T, fixture string, wantLines []string, args ...string) {
	t.Helper()
	out := requireTinySuccess(t, runTinyFile(t, fixturePath(fixture), args...))
	want := strings.Join(append(wantLines, ""), "\n")
	if out != want {
		t.Fatalf("unexpected output from %s:\nwant:\n%q\ngot:\n%q", fixture, want, out)
	}
}

// TestDBSqlite covers the full db module surface on an in-memory SQLite
// database: positional and named params, run/query/get, prepared statements,
// transactions, NULL handling, and catchable errors. Pure Go, no external
// services, so it always runs.
func TestDBSqlite(t *testing.T) {
	assertTinyOutput(t, "db_sqlite.tiny", []string{
		"run rowid=1 changes=1",
		"tx committed",
		"Linus=54",
		"Edsger=60",
		"Grace=85",
		"get=Ada",
		"missing=null",
		"count=4",
		"caught error",
		"done",
	})
}

// TestDBPostgres exercises the postgres driver, including ?/:name -> $N
// placeholder rebinding and RETURNING. It is skipped unless TINY_DB_PG_DSN is
// set; CI provides a postgres service container.
func TestDBPostgres(t *testing.T) {
	if os.Getenv("TINY_DB_PG_DSN") == "" {
		t.Skip("set TINY_DB_PG_DSN to run the postgres db module test")
	}
	assertTinyOutput(t, "db_postgres.tiny", []string{
		"returning=Edsger",
		"Linus=54",
		"Edsger=60",
		"Grace=85",
		"count=4",
		"done",
	})
}

// TestDBMySQL exercises the mysql driver, including lastInsertRowid and named
// params. It is skipped unless TINY_DB_MYSQL_DSN is set; CI provides a mysql
// service container.
func TestDBMySQL(t *testing.T) {
	if os.Getenv("TINY_DB_MYSQL_DSN") == "" {
		t.Skip("set TINY_DB_MYSQL_DSN to run the mysql db module test")
	}
	assertTinyOutput(t, "db_mysql.tiny", []string{
		"run rowid=1",
		"Linus=54",
		"Edsger=60",
		"Grace=85",
		"count=4",
		"done",
	})
}
