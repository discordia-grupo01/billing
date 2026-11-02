package config

import (
	"fmt"
	"os"
)

type Config struct {
	Environment string
	Host        string
	Port        string

	MPAccessToken string
}

func Load() (Config, error) {
	c := Config{
		Environment: getenv("ENVIRONMENT", "development"),
		Host:        getenv("HOST", "0.0.0.0"),
		Port:        getenv("PORT", "8080"),

		MPAccessToken: os.Getenv("MP_ACCESS_TOKEN"),
	}

	if c.MPAccessToken == "" {
		return Config{}, fmt.Errorf("missing required environment variables: MP_ACCESS_TOKEN")
	}

	return c, nil
}

func (c Config) IsProduction() bool { return c.Environment == "production" }

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
