// Package pgtest gives each Postgres-backed test package a database of its own.
//
// go test runs packages in parallel, one process each. With one shared
// database, pgoffer's and postgresstore's contract tests truncated and migrated
// tables under each other: TRUNCATE takes ACCESS EXCLUSIVE, so one package's
// setup waited on the other's open work until its 15s deadline (nagus-0wj,
// pipeline #2646: "truncate: timeout: context deadline exceeded" in tests the
// change under test did not touch). A database per package removes the
// contention instead of tuning the timeout around it.
package pgtest

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// EnvDSN names the variable holding the base DSN (CI and local runs).
const EnvDSN = "NAGUS_TEST_POSTGRES_DSN"

// DSN returns a DSN for the database <base>_<name>, creating it if needed,
// where <base> is the database named in NAGUS_TEST_POSTGRES_DSN. The test is
// skipped when that variable is unset, so machines without Postgres stay green.
// The role in the base DSN must be allowed to CREATE DATABASE (the official
// postgres image's POSTGRES_USER is a superuser).
func DSN(t testing.TB, name string) string {
	t.Helper()
	base := os.Getenv(EnvDSN)
	if base == "" {
		t.Skip("set " + EnvDSN + " to run postgres contract tests")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("pgtest: parse %s: %v", EnvDSN, err)
	}
	db := strings.TrimPrefix(u.Path, "/") + "_" + name

	// Wait for the server: CI's postgres service container starts alongside
	// the job, and the first package to run can get there before it accepts
	// connections (pipeline #2660: "connection refused" at 0.00s).
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var conn *pgx.Conn
	for {
		conn, err = pgx.Connect(ctx, base)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("pgtest: connect (waited for the server): %v", err)
		case <-time.After(time.Second):
		}
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); err != nil {
		var pgErr *pgconn.PgError
		// 42P04 duplicate_database: an earlier test in this package made it.
		if !errors.As(err, &pgErr) || pgErr.Code != "42P04" {
			t.Fatalf("pgtest: create database %s: %v", db, err)
		}
	}
	u.Path = "/" + db
	return u.String()
}

// LogActivity logs what else is running in the test database and who blocks
// whom. Call it when a setup statement times out: a lock wait is otherwise a
// bare "context deadline exceeded" that names nobody (nagus-0wj).
func LogActivity(t testing.TB, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Logf("pgtest: activity: connect: %v", err)
		return
	}
	defer func() { _ = conn.Close(context.Background()) }()
	rows, err := conn.Query(ctx, `
		SELECT pid, state, coalesce(wait_event_type, ''), coalesce(wait_event, ''),
		       pg_blocking_pids(pid)::text, left(query, 120)
		FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()`)
	if err != nil {
		t.Logf("pgtest: activity: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var pid int32
		var state, waitType, wait, blockers, query string
		if err := rows.Scan(&pid, &state, &waitType, &wait, &blockers, &query); err != nil {
			t.Logf("pgtest: activity scan: %v", err)
			return
		}
		t.Logf("pgtest: pid=%d state=%s wait=%s/%s blocked_by=%s query=%q", pid, state, waitType, wait, blockers, query)
	}
}
