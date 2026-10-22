// Package migrations holds the goose SQL migrations and the helpers that apply them.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

// FS contains the migration files.
//
//go:embed *.sql
var FS embed.FS

func provider(db *sql.DB) (*goose.Provider, error) {
	return goose.NewProvider(goose.DialectPostgres, db, FS)
}

// Up applies every pending migration.
func Up(ctx context.Context, db *sql.DB) error {
	p, err := provider(db)
	if err != nil {
		return err
	}
	_, err = p.Up(ctx)
	return err
}

// Down rolls back the most recent migration.
func Down(ctx context.Context, db *sql.DB) error {
	p, err := provider(db)
	if err != nil {
		return err
	}
	_, err = p.Down(ctx)
	return err
}

// Reset rolls every migration back.
func Reset(ctx context.Context, db *sql.DB) error {
	p, err := provider(db)
	if err != nil {
		return err
	}
	_, err = p.DownTo(ctx, 0)
	return err
}

// Version reports the current schema version (0 when nothing is applied).
func Version(ctx context.Context, db *sql.DB) (int64, error) {
	p, err := provider(db)
	if err != nil {
		return 0, err
	}
	return p.GetDBVersion(ctx)
}

// Status returns one line per migration, marking the applied ones.
func Status(ctx context.Context, db *sql.DB) ([]string, error) {
	p, err := provider(db)
	if err != nil {
		return nil, err
	}
	st, err := p.Status(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(st))
	for _, s := range st {
		state := "pending"
		if s.State == goose.StateApplied {
			state = "applied " + s.AppliedAt.Format("2006-01-02 15:04:05")
		}
		out = append(out, fmt.Sprintf("%05d  %-40s %s", s.Source.Version, s.Source.Path, state))
	}
	return out, nil
}
