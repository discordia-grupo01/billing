package handler

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/service"
)

// PremiumSimulation is what the dev routes need from service.PremiumSimulator.
type PremiumSimulation interface {
	SimulateCharge(ctx context.Context, userID uuid.UUID, approved bool) (service.PremiumView, error)
	SimulateExpiry(ctx context.Context, userID uuid.UUID) (service.PremiumView, error)
}

// DevHandler exposes the Premium simulators. It is only mounted with
// BILLING_DEV_TOOLS=true and is never published in Kong, so it has no auth:
// the user comes in the body.
type DevHandler struct {
	simulator PremiumSimulation
	log       *slog.Logger
}

func NewDevHandler(simulator PremiumSimulation, logger *slog.Logger) *DevHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &DevHandler{simulator: simulator, log: logger}
}

func (h *DevHandler) Register(r gin.IRouter) {
	g := r.Group("/dev/billing")
	g.POST("/simulate-charge", h.simulateCharge)
	g.POST("/simulate-expiry", h.simulateExpiry)
}

type simulateChargeRequest struct {
	UserID  uuid.UUID `json:"user_id" binding:"required"`
	Outcome string    `json:"outcome" binding:"required,oneof=approved rejected"`
}

// simulateCharge runs a monthly charge of the user's subscription: approved
// renews Premium (CA3), rejected doesn't (first half of CA4).
func (h *DevHandler) simulateCharge(c *gin.Context) {
	var req simulateChargeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, CodeInvalidRequest, invalidRequestMessage)
		return
	}
	view, err := h.simulator.SimulateCharge(c.Request.Context(), req.UserID, req.Outcome == "approved")
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	c.JSON(http.StatusOK, toPremiumResponse(view))
}

type simulateExpiryRequest struct {
	UserID uuid.UUID `json:"user_id" binding:"required"`
}

// simulateExpiry ends the paid period now, as when a renewal failed or a
// cancelled subscription reaches its date (second half of CA4).
func (h *DevHandler) simulateExpiry(c *gin.Context) {
	var req simulateExpiryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, CodeInvalidRequest, invalidRequestMessage)
		return
	}
	view, err := h.simulator.SimulateExpiry(c.Request.Context(), req.UserID)
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	c.JSON(http.StatusOK, toPremiumResponse(view))
}
