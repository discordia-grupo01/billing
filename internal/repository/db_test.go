package repository

import (
	"context"
	"testing"
)

func TestNewPoolInvalidDSN(t *testing.T) {
	if _, err := NewPool(context.Background(), "port=not-a-number"); err == nil {
		t.Fatal("expected error for invalid DSN")
	}
}

func TestNewPoolUnreachableDatabase(t *testing.T) {
	dsn := "host=127.0.0.1 port=1 dbname=x user=x password=x sslmode=disable connect_timeout=1"
	if _, err := NewPool(context.Background(), dsn); err == nil {
		t.Fatal("expected error when the database is unreachable")
	}
}
