// Command migrate applies the database migrations.
//
//	migrate up        apply all pending migrations
//	migrate down      roll back the latest migration
//	migrate reset     roll back every migration
//	migrate status    list migrations and whether they are applied
//	migrate version   print the current schema version
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/sky-walkerx/trequila/migrations"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: migrate up|down|reset|status|version")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(cmd string) error {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is not set")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := waitForDB(ctx, db); err != nil {
		return err
	}

	switch cmd {
	case "up":
		if err := migrations.Up(ctx, db); err != nil {
			return err
		}
	case "down":
		if err := migrations.Down(ctx, db); err != nil {
			return err
		}
	case "reset":
		if err := migrations.Reset(ctx, db); err != nil {
			return err
		}
	case "status":
		lines, err := migrations.Status(ctx, db)
		if err != nil {
			return err
		}
		for _, l := range lines {
			fmt.Println(l)
		}
		return nil
	case "version":
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
	v, err := migrations.Version(ctx, db)
	if err != nil {
		return err
	}
	fmt.Println("schema version:", v)
	return nil
}

func waitForDB(ctx context.Context, db *sql.DB) error {
	var err error
	for i := 0; i < 30; i++ {
		if err = db.PingContext(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("database not reachable: %w", err)
}
