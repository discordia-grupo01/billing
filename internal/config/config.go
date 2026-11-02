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

	MPAccessToken string
	// Sandbox only: MP rejects subscriptions whose payer is not the test buyer.
	MPTestPayerEmail string
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

		MPAccessToken:    os.Getenv("MP_ACCESS_TOKEN"),
		MPTestPayerEmail: os.Getenv("MP_TEST_PAYER_EMAIL"),
	}

	var missing []string
	for k, v := range map[string]string{
		"DATABASE_HOST":     c.DatabaseHost,
		"DATABASE_NAME":     c.DatabaseName,
		"DATABASE_USER":     c.DatabaseUser,
		"DATABASE_PASSWORD": c.DatabasePassword,
		"MP_ACCESS_TOKEN":   c.MPAccessToken,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
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
