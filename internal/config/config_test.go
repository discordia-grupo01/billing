package config

import "testing"

func TestLoadRequiresAccessToken(t *testing.T) {
	t.Setenv("MP_ACCESS_TOKEN", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected error when MP_ACCESS_TOKEN is missing")
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("MP_ACCESS_TOKEN", "token")
	t.Setenv("ENVIRONMENT", "")
	t.Setenv("HOST", "")
	t.Setenv("PORT", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Environment != "development" || c.Host != "0.0.0.0" || c.Port != "8080" {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.IsProduction() {
		t.Error("should not be production by default")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("MP_ACCESS_TOKEN", "token")
	t.Setenv("ENVIRONMENT", "production")

	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.IsProduction() || c.MPAccessToken != "token" {
		t.Errorf("overrides not applied: %+v", c)
	}
}
