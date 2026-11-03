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
	t.Setenv("JWT_SECRET", "jwt")
	t.Setenv("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/")
	t.Setenv("BILLING_EVENTS_HMAC_SECRET", "events")
	t.Setenv("MP_WEBHOOK_SECRET", "whsec")
	t.Setenv("CHECKOUT_RETURN_URL", "https://web.test/")
}

func TestLoadReportsAllMissingVariables(t *testing.T) {
	for _, k := range []string{"DATABASE_HOST", "DATABASE_NAME", "DATABASE_USER", "DATABASE_PASSWORD",
		"MP_ACCESS_TOKEN", "JWT_SECRET", "MP_WEBHOOK_SECRET", "CHECKOUT_RETURN_URL",
		"RABBITMQ_URL", "BILLING_EVENTS_HMAC_SECRET"} {
		t.Setenv(k, "")
	}

	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	want := "BILLING_EVENTS_HMAC_SECRET, CHECKOUT_RETURN_URL, DATABASE_HOST, DATABASE_NAME, DATABASE_PASSWORD, " +
		"DATABASE_USER, JWT_SECRET, MP_ACCESS_TOKEN, MP_WEBHOOK_SECRET, RABBITMQ_URL"
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
	if c.JWTSecret != "jwt" || c.MPWebhookSecret != "whsec" || c.BillingEventsHMACSecret != "events" ||
		c.RabbitMQURL != "amqp://guest:guest@localhost:5672/" {
		t.Errorf("secrets not loaded: %+v", c)
	}
	if c.CheckoutReturnURL != "https://web.test" {
		t.Errorf("CheckoutReturnURL = %q, want it without the trailing slash", c.CheckoutReturnURL)
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
