package config

import (
	"strings"
	"testing"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_HOST", "db.local")
	t.Setenv("DATABASE_NAME", "billing")
	t.Setenv("DATABASE_USER", "user")
	t.Setenv("DATABASE_PASSWORD", "secret")
	t.Setenv("MP_ACCESS_TOKEN", "token")
}

func TestLoadReportsAllMissingVariables(t *testing.T) {
	for _, k := range []string{"DATABASE_HOST", "DATABASE_NAME", "DATABASE_USER", "DATABASE_PASSWORD", "MP_ACCESS_TOKEN"} {
		t.Setenv(k, "")
	}

	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	want := "DATABASE_HOST, DATABASE_NAME, DATABASE_PASSWORD, DATABASE_USER, MP_ACCESS_TOKEN"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to list %q", err, want)
	}
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)
	for _, k := range []string{"ENVIRONMENT", "HOST", "PORT", "DATABASE_PORT", "DATABASE_SSLMODE"} {
		t.Setenv(k, "")
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Environment != "development" || c.Host != "0.0.0.0" || c.Port != "8080" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.DatabasePort != "5432" || c.DatabaseSSLMode != "disable" {
		t.Errorf("unexpected database defaults: port %q sslmode %q", c.DatabasePort, c.DatabaseSSLMode)
	}
	if c.IsProduction() {
		t.Error("should not be production by default")
	}
}

func TestLoadOverrides(t *testing.T) {
	setRequired(t)
	t.Setenv("ENVIRONMENT", "production")
	t.Setenv("DATABASE_SSLMODE", "require")
	t.Setenv("MP_TEST_PAYER_EMAIL", "test_user_1@testuser.com")

	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.IsProduction() || c.DatabaseSSLMode != "require" || c.MPAccessToken != "token" {
		t.Errorf("overrides not applied: %+v", c)
	}
	if c.MPTestPayerEmail != "test_user_1@testuser.com" {
		t.Errorf("MPTestPayerEmail = %q", c.MPTestPayerEmail)
	}
}

func TestPostgresDSN(t *testing.T) {
	c := Config{
		DatabaseHost: "h", DatabasePort: "5432", DatabaseName: "n",
		DatabaseUser: "u", DatabasePassword: "p", DatabaseSSLMode: "require",
	}
	want := "host=h port=5432 dbname=n user=u password=p sslmode=require"
	if got := c.PostgresDSN(); got != want {
		t.Errorf("PostgresDSN() = %q, want %q", got, want)
	}
}
