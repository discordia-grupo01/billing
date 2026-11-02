package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/discordia-grupo01/billing/internal/config"
	"github.com/discordia-grupo01/billing/internal/repository"
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

	router := gin.Default()

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
