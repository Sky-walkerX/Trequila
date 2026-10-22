package migrations_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sky-walkerx/trequila/internal/testdb"
	"github.com/sky-walkerx/trequila/migrations"
)

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var reg sql.NullString
	if err := db.QueryRow(`SELECT to_regclass($1)::text`, name).Scan(&reg); err != nil {
		t.Fatal(err)
	}
	return reg.Valid
}

func TestMigrationsApplyRollBackAndApplyAgain(t *testing.T) {
	db := testdb.Empty(t)
	ctx := context.Background()

	if err := migrations.Up(ctx, db); err != nil {
		t.Fatalf("up: %v", err)
	}
	for _, table := range []string{"users", "trips", "provider_keys", "conversations"} {
		if !hasTable(t, db, table) {
			t.Errorf("table %s missing after up", table)
		}
	}

	if err := migrations.Reset(ctx, db); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if hasTable(t, db, "trips") {
		t.Error("trips still exists after reset")
	}

	if err := migrations.Up(ctx, db); err != nil {
		t.Fatalf("second up: %v", err)
	}
	v, err := migrations.Version(ctx, db)
	if err != nil || v < 1 {
		t.Fatalf("version = %d, %v", v, err)
	}
}

func TestBaselineAdoptsADatabaseThatAlreadyHasTheLegacySchema(t *testing.T) {
	db := testdb.Empty(t)
	ctx := context.Background()
	legacy := `
		CREATE TABLE users (id TEXT PRIMARY KEY, email VARCHAR(255) UNIQUE NOT NULL, name VARCHAR(255),
			password_hash VARCHAR(255), provider VARCHAR(50), provider_id VARCHAR(255), avatar_url TEXT,
			created_at TIMESTAMPTZ DEFAULT now(), updated_at TIMESTAMPTZ DEFAULT now());
		CREATE TABLE trips (request_id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id),
			source VARCHAR(255) NOT NULL, destination VARCHAR(255) NOT NULL, start_date DATE NOT NULL,
			end_date DATE NOT NULL, budget INTEGER, status VARCHAR(50), provider TEXT, model TEXT,
			error_reason TEXT, itinerary JSONB, images JSONB, places JSONB, vibes JSONB, route JSONB,
			share_code VARCHAR(32) UNIQUE, created_at TIMESTAMPTZ DEFAULT now(), updated_at TIMESTAMPTZ DEFAULT now());
		CREATE TABLE trip_collaborators (trip_id TEXT, user_id TEXT);
		INSERT INTO users (id, email) VALUES ('u1', 'a@example.com');
		INSERT INTO trips (request_id, user_id, source, destination, start_date, end_date)
			VALUES ('t1', 'u1', 'A', 'B', '2026-01-01', '2026-01-02');`
	if _, err := db.ExecContext(ctx, legacy); err != nil {
		t.Fatal(err)
	}

	if err := migrations.Up(ctx, db); err != nil {
		t.Fatalf("up over legacy schema: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM trips`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("existing trip lost: n=%d err=%v", n, err)
	}
	if hasTable(t, db, "trip_collaborators") {
		t.Error("legacy trip_collaborators should have been dropped")
	}
	var cols int
	db.QueryRow(`SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'trips' AND column_name IN ('vibes','share_code')`).Scan(&cols)
	if cols != 0 {
		t.Errorf("legacy columns still present: %d", cols)
	}
}
