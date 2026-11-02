package main

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/discordia-grupo01/billing/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.Default()

	// /livez: solo confirma que el proceso esta vivo. No toca la BDD.
	router.GET("/livez", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	// /readyz: confirma que el servicio puede operar (chequear BDD aca).
	router.GET("/readyz", func(c *gin.Context) {
		// TODO: chequear conexion a Postgres antes de responder 200
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	router.GET("/", func(c *gin.Context) {
		c.String(http.StatusOK, "discordia-billing up")
	})

	addr := cfg.Host + ":" + cfg.Port
	log.Printf("listening on %s", addr)
	log.Fatal(router.Run(addr))
}
