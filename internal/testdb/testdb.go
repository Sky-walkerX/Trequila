// Package testdb gives tests an isolated Postgres schema.
//
// Tests that need a database call New (migrated) or Empty (bare). They are skipped
// unless TEST_DATABASE_URL points at a Postgres server, so `go test ./...` still
// works with no services running; CI and `make test-int` set it.
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sky-walkerx/trequila/migrations"
)

// URL returns the server URL from TEST_DATABASE_URL, or skips the test.
func URL(tb testing.TB) string {
	tb.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		tb.Skip("TEST_DATABASE_URL not set; skipping Postgres-backed test")
	}
	return u
}

// Empty returns a pool whose connections all use a fresh, empty schema that is
// dropped when the test ends.
func Empty(tb testing.TB) *sql.DB {
	db, _ := EmptyWithURL(tb)
	return db
}

// EmptyWithURL is Empty that also returns a connection URL bound to the same schema.
func EmptyWithURL(tb testing.TB) (*sql.DB, string) {
	tb.Helper()
	base := URL(tb)

	admin, err := sql.Open("pgx", base)
	if err != nil {
		tb.Fatalf("testdb: open admin: %v", err)
	}
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		tb.Fatal(err)
	}
	schema := "t_" + hex.EncodeToString(raw[:])
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		_ = admin.Close()
		tb.Fatalf("testdb: create schema: %v", err)
	}

	u, err := url.Parse(base)
	if err != nil {
		tb.Fatalf("testdb: parse TEST_DATABASE_URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	db, err := sql.Open("pgx", u.String())
	if err != nil {
		tb.Fatalf("testdb: open: %v", err)
	}
	tb.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})
	return db, u.String()
}

// New returns a pool on a fresh schema with every migration applied.
func New(tb testing.TB) *sql.DB {
	db, _ := NewWithURL(tb)
	return db
}

// NewWithURL is New that also returns a connection URL bound to the same schema.
func NewWithURL(tb testing.TB) (*sql.DB, string) {
	tb.Helper()
	db, u := EmptyWithURL(tb)
	if err := migrations.Up(context.Background(), db); err != nil {
		tb.Fatalf("testdb: migrate: %v", err)
	}
	return db, u
}
