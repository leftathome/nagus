package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/url"

	"github.com/leftathome/nagus/internal/store"
	"github.com/leftathome/nagus/internal/store/postgresstore"
	"github.com/leftathome/nagus/internal/store/sqlitestore"
)

// storeFlags holds the backend-selection flags shared by every subcommand, so
// the CLI and the Helm-deployed `serve` open the store the same way. Backend is
// "sqlite" (default, single file) or "postgres" (shared CNPG cluster). Postgres
// connection parameters come from individual flags/env so the password can be
// injected from a Secret (secretKeyRef) without ever appearing in values.yaml.
type storeFlags struct {
	backend    *string
	sqlitePath *string
	pgHost     *string
	pgPort     *string
	pgDB       *string
	pgUser     *string
	pgPassword *string
	pgSSLMode  *string
}

func registerStoreFlags(fs *flag.FlagSet) *storeFlags {
	return &storeFlags{
		backend:    fs.String("store-backend", envOr("NAGUS_STORE_BACKEND", "sqlite"), "store backend: sqlite | postgres"),
		sqlitePath: fs.String("db", envOr("NAGUS_DB", "nagus.db"), "sqlite store path (sqlite backend)"),
		pgHost:     fs.String("pg-host", envOr("NAGUS_PG_HOST", "postgres-rw.databases-app.svc.cluster.local"), "postgres host (postgres backend)"),
		pgPort:     fs.String("pg-port", envOr("NAGUS_PG_PORT", "5432"), "postgres port"),
		pgDB:       fs.String("pg-db", envOr("NAGUS_PG_DB", "nagus"), "postgres database"),
		pgUser:     fs.String("pg-user", envOr("NAGUS_PG_USER", ""), "postgres user (inject from a Secret)"),
		pgPassword: fs.String("pg-password", envOr("NAGUS_PG_PASSWORD", ""), "postgres password (inject from a Secret; never commit)"),
		pgSSLMode:  fs.String("pg-sslmode", envOr("NAGUS_PG_SSLMODE", "require"), "postgres sslmode"),
	}
}

// open opens the configured store and returns it plus a close func. The close
// func is always non-nil (a no-op when nothing needs closing).
func (sf *storeFlags) open(ctx context.Context) (store.Store, func(), error) {
	switch *sf.backend {
	case "sqlite":
		st, err := sqlitestore.New(*sf.sqlitePath)
		if err != nil {
			return nil, func() {}, fmt.Errorf("open sqlite store %q: %w", *sf.sqlitePath, err)
		}
		return st, func() { _ = st.Close() }, nil
	case "postgres":
		dsn := buildPostgresDSN(*sf.pgHost, *sf.pgPort, *sf.pgDB, *sf.pgUser, *sf.pgPassword, *sf.pgSSLMode)
		st, err := postgresstore.New(ctx, dsn)
		if err != nil {
			return nil, func() {}, fmt.Errorf("open postgres store at %s:%s/%s: %w", *sf.pgHost, *sf.pgPort, *sf.pgDB, err)
		}
		return st, func() { st.Close() }, nil
	default:
		return nil, func() {}, fmt.Errorf("unknown store backend %q (want sqlite or postgres)", *sf.backend)
	}
}

// buildPostgresDSN assembles a libpq/pgx URL. Credentials are only ever passed
// in, never logged; callers derive the password from a Secret at runtime.
func buildPostgresDSN(host, port, db, user, password, sslmode string) string {
	u := url.URL{Scheme: "postgres", Host: net.JoinHostPort(host, port), Path: "/" + db}
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	q := url.Values{}
	if sslmode != "" {
		q.Set("sslmode", sslmode)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
