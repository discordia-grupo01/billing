package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

type Config struct {
	Environment string
	Host        string
	Port        string

	DatabaseHost     string
	DatabasePort     string
	DatabaseName     string
	DatabaseUser     string
	DatabasePassword string
	DatabaseSSLMode  string

	// Same HS256 secret identify signs access tokens with.
	JWTSecret string

	RabbitMQURL string
	// Signs billing.premium_updated; identify and messaging verify it.
	BillingEventsHMACSecret string

	MPAccessToken string
	// Secret signature of the MP webhook, used to validate x-signature.
	MPWebhookSecret string
	// Sandbox only: MP rejects subscriptions whose payer is not the test buyer.
	MPTestPayerEmail string
	// Public base URL of the web client. MP sends the buyer back to
	// <base>/billing/return after paying; it can't be localhost.
	CheckoutReturnURL string

	// Mounts /dev/billing/simulate-*, which fake renewals and expiries.
	// Never in production.
	DevTools bool
}

func Load() (Config, error) {
	c := Config{
		Environment: getenv("ENVIRONMENT", "development"),
		Host:        getenv("HOST", "0.0.0.0"),
		Port:        getenv("PORT", "8080"),

		DatabaseHost:     os.Getenv("DATABASE_HOST"),
		DatabasePort:     getenv("DATABASE_PORT", "5432"),
		DatabaseName:     os.Getenv("DATABASE_NAME"),
		DatabaseUser:     os.Getenv("DATABASE_USER"),
		DatabasePassword: os.Getenv("DATABASE_PASSWORD"),
		DatabaseSSLMode:  getenv("DATABASE_SSLMODE", "disable"),

		JWTSecret: os.Getenv("JWT_SECRET"),

		RabbitMQURL:             os.Getenv("RABBITMQ_URL"),
		BillingEventsHMACSecret: os.Getenv("BILLING_EVENTS_HMAC_SECRET"),

		MPAccessToken:     os.Getenv("MP_ACCESS_TOKEN"),
		MPWebhookSecret:   os.Getenv("MP_WEBHOOK_SECRET"),
		MPTestPayerEmail:  os.Getenv("MP_TEST_PAYER_EMAIL"),
		CheckoutReturnURL: strings.TrimRight(os.Getenv("CHECKOUT_RETURN_URL"), "/"),

		DevTools: os.Getenv("BILLING_DEV_TOOLS") == "true",
	}

	var missing []string
	for k, v := range map[string]string{
		"DATABASE_HOST":              c.DatabaseHost,
		"DATABASE_NAME":              c.DatabaseName,
		"DATABASE_USER":              c.DatabaseUser,
		"DATABASE_PASSWORD":          c.DatabasePassword,
		"JWT_SECRET":                 c.JWTSecret,
		"RABBITMQ_URL":               c.RabbitMQURL,
		"BILLING_EVENTS_HMAC_SECRET": c.BillingEventsHMACSecret,
		"MP_ACCESS_TOKEN":            c.MPAccessToken,
		"MP_WEBHOOK_SECRET":          c.MPWebhookSecret,
		"CHECKOUT_RETURN_URL":        c.CheckoutReturnURL,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	if c.DevTools && c.IsProduction() {
		return Config{}, fmt.Errorf("BILLING_DEV_TOOLS can't be enabled in production: it lets anyone renew or expire Premium")
	}

	return c, nil
}

func (c Config) PostgresDSN() string {
	return fmt.Sprintf(
		"host=%s port=%s dbname=%s user=%s password=%s sslmode=%s",
		c.DatabaseHost, c.DatabasePort, c.DatabaseName, c.DatabaseUser, c.DatabasePassword, c.DatabaseSSLMode,
	)
}

func (c Config) IsProduction() bool { return c.Environment == "production" }

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
