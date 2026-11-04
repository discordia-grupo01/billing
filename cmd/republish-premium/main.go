// republish-premium writes a billing.premium_updated with the current state of
// every user into the outbox. The relay of the billing running against the same
// database publishes them. Use it after a consumer's queue is created.
package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/discordia-grupo01/billing/internal/config"
	"github.com/discordia-grupo01/billing/internal/repository"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	pool, err := repository.NewPool(ctx, cfg.PostgresDSN())
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer pool.Close()

	written, err := repository.NewPremiumRepository(pool).RepublishAll(ctx)
	log.Printf("wrote %d billing.premium_updated events to the outbox", written)
	if err != nil {
		log.Fatalf("stopped early, it is safe to run again: %v", err)
	}
}
