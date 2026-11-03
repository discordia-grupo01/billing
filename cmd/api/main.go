package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/discordia-grupo01/billing/internal/config"
	"github.com/discordia-grupo01/billing/internal/handler"
	"github.com/discordia-grupo01/billing/internal/mercadopago"
	"github.com/discordia-grupo01/billing/internal/middleware"
	"github.com/discordia-grupo01/billing/internal/repository"
	"github.com/discordia-grupo01/billing/internal/service"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	pool, err := repository.NewPool(context.Background(), cfg.PostgresDSN())
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer pool.Close()

	gateway, err := mercadopago.NewClient(cfg.MPAccessToken)
	if err != nil {
		log.Fatalf("mercadopago: %v", err)
	}

	logger := slog.Default()
	returnURL := cfg.CheckoutReturnURL + "/billing/return"
	points := service.NewPointsService(repository.NewPointsRepository(pool), gateway, service.ReturnURLs{
		Success: returnURL + "?kind=points&result=success",
		Failure: returnURL + "?kind=points&result=failure",
		Pending: returnURL + "?kind=points&result=pending",
	}, logger)
	premium := service.NewPremiumService(repository.NewPremiumRepository(pool), gateway, service.PremiumConfig{
		BackURL:        returnURL + "?kind=premium",
		TestPayerEmail: cfg.MPTestPayerEmail,
	}, logger)

	router := gin.Default()
	handler.NewBillingHandler(points, premium, logger).Register(router, middleware.Auth([]byte(cfg.JWTSecret)))
	handler.NewWebhookHandler(points, premium, cfg.MPWebhookSecret, logger).Register(router)

	// /livez: solo confirma que el proceso esta vivo. No toca la BDD.
	router.GET("/livez", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	// /readyz: confirma que el servicio puede operar (llega a la BDD).
	router.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	router.GET("/", func(c *gin.Context) {
		c.String(http.StatusOK, "discordia-billing up")
	})

	addr := cfg.Host + ":" + cfg.Port
	log.Printf("listening on %s", addr)
	log.Fatal(router.Run(addr))
}
