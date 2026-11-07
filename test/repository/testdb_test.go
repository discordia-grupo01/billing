package repository_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/discordia-grupo01/billing/internal/repository"
)

// These tests drop and recreate every table. They read TEST_DATABASE_URL on
// purpose instead of DATABASE_*: the Makefile exports .env, which points to
// the real Supabase database.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set: skipping integration test")
	}
	if strings.Contains(dsn, "supabase") {
		t.Fatal("TEST_DATABASE_URL points to Supabase; use a disposable local Postgres")
	}

	ctx := context.Background()
	pool, err := repository.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS server_boosts`); err != nil {
		t.Fatalf("drop server_boosts: %v", err)
	}
	applyMigration(t, pool, "0001_init.down.sql")
	applyMigration(t, pool, "0001_init.up.sql")
	applyMigration(t, pool, "0002_reconcile_reversals.up.sql")
	applyMigration(t, pool, "0003_checkout_requests.down.sql")
	applyMigration(t, pool, "0003_checkout_requests.up.sql")
	applyMigration(t, pool, "0004_server_boosts.up.sql")
	applyMigration(t, pool, "0005_duplicate_charges.up.sql")
	applyMigration(t, pool, "0006_outbox_traceparent.up.sql")
	return pool
}

func applyMigration(t *testing.T, pool *pgxpool.Pool, file string) {
	t.Helper()
	sql, err := os.ReadFile("../../migrations/" + file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if _, err := pool.Exec(context.Background(), string(sql)); err != nil {
		t.Fatalf("apply %s: %v", file, err)
	}
}
