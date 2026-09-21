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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("pgtest: connect: %v", err)
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
