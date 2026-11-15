package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const readyzTimeout = 2 * time.Second

// Pinger is what /readyz needs from the database pool.
type Pinger interface {
	Ping(ctx context.Context) error
}

// HealthStatus is the body of /readyz.
type HealthStatus struct {
	Status string `json:"status" enums:"ready,unavailable"`
} // @name HealthStatus

// Livez only confirms the process is alive. It never touches the database.
//
// @Summary      Liveness check
// @Description  Confirma que el proceso está vivo. No consulta ninguna dependencia.
// @Tags         Health
// @Produce      plain
// @Success      200  {string}  string  "ok"
// @Router       /livez [get]
func Livez(c *gin.Context) {
	c.String(http.StatusOK, "ok")
}

// Readyz confirms the service can operate: it reaches the database.
//
// @Summary      Readiness check
// @Description  Confirma que el servicio puede operar: llega a la base de datos.
// @Tags         Health
// @Produce      json
// @Success      200  {object}  HealthStatus  "Listo para recibir tráfico"
// @Failure      503  {object}  HealthStatus  "La base de datos no responde"
// @Router       /readyz [get]
func Readyz(db Pinger) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), readyzTimeout)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, HealthStatus{Status: "unavailable"})
			return
		}
		c.JSON(http.StatusOK, HealthStatus{Status: "ready"})
	}
}
