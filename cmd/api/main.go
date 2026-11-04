package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/discordia-grupo01/billing/internal/config"
	"github.com/discordia-grupo01/billing/internal/handler"
	"github.com/discordia-grupo01/billing/internal/jobs"
	"github.com/discordia-grupo01/billing/internal/mercadopago"
	"github.com/discordia-grupo01/billing/internal/middleware"
	"github.com/discordia-grupo01/billing/internal/outbox"
	"github.com/discordia-grupo01/billing/internal/rabbitmq"
	"github.com/discordia-grupo01/billing/internal/repository"
	"github.com/discordia-grupo01/billing/internal/service"
)

// How often each job runs; see internal/jobs.
const (
	outboxInterval            = 2 * time.Second
	outboxTimeout             = 30 * time.Second
	expiryInterval            = time.Minute
	purchaseReconcileInterval = 15 * time.Minute
	premiumReconcileInterval  = time.Hour
	shutdownTimeout           = 20 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	pool, err := repository.NewPool(ctx, cfg.PostgresDSN())
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
	premiumRepo := repository.NewPremiumRepository(pool)
	premium := service.NewPremiumService(premiumRepo, gateway, service.PremiumConfig{
		BackURL:        returnURL + "?kind=premium",
		TestPayerEmail: cfg.MPTestPayerEmail,
	}, logger)

	router := gin.Default()
	handler.NewBillingHandler(points, premium, logger).Register(router, middleware.Auth([]byte(cfg.JWTSecret)))
	handler.NewWebhookHandler(points, premium, cfg.MPWebhookSecret, logger).Register(router)
	if cfg.DevTools {
		logger.Warn("BILLING_DEV_TOOLS is on: /dev/billing/simulate-* can renew or expire anyone's Premium")
		simulator := service.NewPremiumSimulator(premium, premiumRepo)
		handler.NewDevHandler(simulator, logger).Register(router)
	}

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

	publisher := rabbitmq.NewPublisher(cfg.RabbitMQURL)
	defer publisher.Close()
	relay := outbox.NewRelay(repository.NewOutboxRepository(pool), publisher, cfg.BillingEventsHMACSecret)

	waitJobs := jobs.Start(ctx, logger,
		jobs.Job{Name: "outbox_relay", Interval: outboxInterval, Timeout: outboxTimeout, Run: relay.Drain},
		jobs.Job{Name: "premium_expiry", Interval: expiryInterval, Run: premium.ExpireDue},
		jobs.Job{Name: "points_reconcile", Interval: purchaseReconcileInterval, Run: points.ReconcilePending},
		jobs.Job{Name: "premium_reconcile", Interval: premiumReconcileInterval, Run: premium.ReconcileSubscriptions},
	)

	srv := &http.Server{Addr: cfg.Host + ":" + cfg.Port, Handler: router, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	waitJobs()
}
