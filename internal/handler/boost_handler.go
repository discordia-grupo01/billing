package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/middleware"
	"github.com/discordia-grupo01/billing/internal/model"
)

// BoostUseCases is what the boost routes need from service.BoostService.
type BoostUseCases interface {
	Request(ctx context.Context, userID, serverID uuid.UUID, idempotencyKey string) (model.Boost, error)
	Get(ctx context.Context, userID, id uuid.UUID) (model.Boost, error)
	List(ctx context.Context, userID uuid.UUID, limit int) ([]model.Boost, error)
	Remove(ctx context.Context, userID, id uuid.UUID) (model.Boost, error)
}

type BoostHandler struct {
	boosts BoostUseCases
	log    *slog.Logger
}

func NewBoostHandler(boosts BoostUseCases, logger *slog.Logger) *BoostHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &BoostHandler{boosts: boosts, log: logger}
}

func (h *BoostHandler) Register(r gin.IRouter, auth gin.HandlerFunc) {
	g := r.Group("/v1/billing/boosts", auth)
	g.POST("", h.request)
	g.GET("", h.list)
	g.GET("/:id", h.get)
	g.DELETE("/:id", h.remove)
}

type boostRequest struct {
	ServerID string `json:"server_id" binding:"required,uuid"`
}

type boostResponse struct {
	ID           uuid.UUID `json:"id"`
	ServerID     uuid.UUID `json:"server_id"`
	Points       int       `json:"points"`
	Status       string    `json:"status"`
	StatusDetail string    `json:"status_detail"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func toBoostResponse(b model.Boost) boostResponse {
	return boostResponse{
		ID: b.ID, ServerID: b.ServerID, Points: b.Points, Status: string(b.Status),
		StatusDetail: b.StatusDetail, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
	}
}

// request answers 202: the boost stays pending until servers decides, and the
// client polls GET /v1/billing/boosts/:id.
func (h *BoostHandler) request(c *gin.Context) {
	var req boostRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, CodeInvalidRequest, invalidRequestMessage)
		return
	}
	key, ok := idempotencyKey(c)
	if !ok {
		return
	}
	boost, err := h.boosts.Request(c.Request.Context(), middleware.UserID(c), uuid.MustParse(req.ServerID), key)
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	c.JSON(http.StatusAccepted, toBoostResponse(boost))
}

func (h *BoostHandler) list(c *gin.Context) {
	limit, ok := parseLimit(c)
	if !ok {
		return
	}
	list, err := h.boosts.List(c.Request.Context(), middleware.UserID(c), limit)
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	out := make([]boostResponse, 0, len(list))
	for _, b := range list {
		out = append(out, toBoostResponse(b))
	}
	c.JSON(http.StatusOK, gin.H{"boosts": out})
}

func (h *BoostHandler) get(c *gin.Context) {
	h.respond(c, h.boosts.Get)
}

func (h *BoostHandler) remove(c *gin.Context) {
	h.respond(c, h.boosts.Remove)
}

func (h *BoostHandler) respond(c *gin.Context, action func(context.Context, uuid.UUID, uuid.UUID) (model.Boost, error)) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		writeError(c, http.StatusNotFound, CodeBoostNotFound, boostNotFoundMessage)
		return
	}
	boost, err := action(c.Request.Context(), middleware.UserID(c), id)
	if errors.Is(err, model.ErrNotFound) {
		writeError(c, http.StatusNotFound, CodeBoostNotFound, boostNotFoundMessage)
		return
	}
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	c.JSON(http.StatusOK, toBoostResponse(boost))
}
