package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/discordia-grupo01/billing/internal/config"
	"github.com/discordia-grupo01/billing/internal/handler"
	"github.com/discordia-grupo01/billing/internal/jobs"
	"github.com/discordia-grupo01/billing/internal/logging"
	"github.com/discordia-grupo01/billing/internal/mercadopago"
	"github.com/discordia-grupo01/billing/internal/middleware"
	"github.com/discordia-grupo01/billing/internal/outbox"
	"github.com/discordia-grupo01/billing/internal/rabbitmq"
	"github.com/discordia-grupo01/billing/internal/repository"
	"github.com/discordia-grupo01/billing/internal/service"
	"github.com/discordia-grupo01/billing/internal/telemetry"
)

// How often each job runs; see internal/jobs.
const (
	outboxInterval            = 2 * time.Second
	outboxTimeout             = 30 * time.Second
	expiryInterval            = time.Minute
	purchaseReconcileInterval = 15 * time.Minute
	paidReconcileInterval     = time.Hour
	premiumReconcileInterval  = time.Hour
	checkoutCleanupInterval   = time.Hour
	boostRepublishInterval    = 5 * time.Minute
	boostCancelInterval       = 15 * time.Minute
	shutdownTimeout           = 20 * time.Second
)

func main() {
	logging.MustSetup("billing")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		logging.Fatalf("config: %v", err)
	}
	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	shutdownTelemetry, err := telemetry.Setup(ctx, "billing")
	if err != nil {
		logging.Fatalf("telemetry: %v", err)
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTelemetry(flushCtx)
	}()

	pool, err := repository.NewPool(ctx, cfg.PostgresDSN())
	if err != nil {
		logging.Fatalf("postgres: %v", err)
	}
	defer pool.Close()

	gateway, err := mercadopago.NewClient(cfg.MPAccessToken)
	if err != nil {
		logging.Fatalf("mercadopago: %v", err)
	}

	logger := slog.Default()
	returnURL := cfg.CheckoutReturnURL + "/billing/return"
	appReturnURL := cfg.CheckoutReturnURL + "/app-link/billing/return"
	points := service.NewPointsService(repository.NewPointsRepository(pool), gateway, pointsReturnURLs(returnURL), logger).
		WithAppReturnURLs(pointsReturnURLs(appReturnURL))
	premiumRepo := repository.NewPremiumRepository(pool)
	premium := service.NewPremiumService(premiumRepo, gateway, service.PremiumConfig{
		BackURL:        returnURL + "?kind=premium",
		AppBackURL:     appReturnURL + "?kind=premium",
		TestPayerEmail: cfg.MPTestPayerEmail,
	}, logger)

	router := gin.New()
	router.Use(telemetry.Middleware(), logging.Middleware(), logging.Recovery())
	checkouts := service.NewCheckoutGuard(repository.NewCheckoutRequestRepository(pool), logger)
	handler.NewBillingHandler(points, premium, checkouts, logger).Register(router, middleware.Auth([]byte(cfg.JWTSecret)))
	handler.NewWebhookHandler(points, premium, cfg.MPWebhookSecret, logger).Register(router)
	boostsService := service.NewBoostService(repository.NewBoostRepository(pool), cfg.BillingEventsHMACSecret, logger)
	handler.NewBoostHandler(boostsService, logger).Register(router, middleware.Auth([]byte(cfg.JWTSecret)))
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
		jobs.Job{Name: "points_paid_reconcile", Interval: paidReconcileInterval, Run: points.ReconcilePaid},
		jobs.Job{Name: "premium_reconcile", Interval: premiumReconcileInterval, Run: premium.ReconcileSubscriptions},
		jobs.Job{Name: "checkout_keys_cleanup", Interval: checkoutCleanupInterval, Run: checkouts.DeleteExpired},
		jobs.Job{Name: "boost_republish", Interval: boostRepublishInterval, Run: boostsService.RepublishPending},
		jobs.Job{Name: "boost_cancel_expired", Interval: boostCancelInterval, Run: boostsService.CancelExpired},
	)

	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		rabbitmq.NewConsumer(cfg.RabbitMQURL, service.ServersEventsQueue, service.ServersEventsRoutingKeys(), boostsService.HandleEvent, logger).Run(ctx)
	}()

	srv := &http.Server{Addr: cfg.Host + ":" + cfg.Port, Handler: router, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		logging.Infof("listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	logging.Infof("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logging.Errorf("http shutdown: %v", err)
	}
	waitJobs()
	<-consumerDone
}

func pointsReturnURLs(base string) service.ReturnURLs {
	return service.ReturnURLs{
		Success: base + "?kind=points&result=success",
		Failure: base + "?kind=points&result=failure",
		Pending: base + "?kind=points&result=pending",
	}
}
