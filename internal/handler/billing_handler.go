package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/middleware"
	"github.com/discordia-grupo01/billing/internal/model"
	"github.com/discordia-grupo01/billing/internal/service"
)

// PointsUseCases is what the points routes need from service.PointsService.
type PointsUseCases interface {
	Packages() []service.PointsPackage
	StartCheckout(ctx context.Context, userID uuid.UUID, packageID string, payer service.CheckoutPayer, returnTo service.ReturnTarget) (service.Checkout, error)
	GetPurchase(ctx context.Context, userID, purchaseID uuid.UUID) (model.PointPurchase, error)
	Balance(ctx context.Context, userID uuid.UUID) (int64, error)
	ListPurchases(ctx context.Context, userID uuid.UUID, limit int) ([]model.PointPurchase, error)
	ListLedger(ctx context.Context, userID uuid.UUID, limit int) ([]model.LedgerEntry, error)
}

// PremiumUseCases is what the Premium routes need from service.PremiumService.
type PremiumUseCases interface {
	StartCheckout(ctx context.Context, userID uuid.UUID, payerEmail string, returnTo service.ReturnTarget) (string, error)
	Cancel(ctx context.Context, userID uuid.UUID) (service.PremiumView, error)
	State(ctx context.Context, userID uuid.UUID, refresh bool) (service.PremiumView, error)
}

// CheckoutRunner is what the checkout routes need from service.CheckoutGuard.
type CheckoutRunner interface {
	Run(ctx context.Context, k model.CheckoutKey, fingerprint string, checkout func(context.Context) (service.Checkout, error)) (service.Checkout, error)
}

const (
	idempotencyKeyHeader    = "Idempotency-Key"
	maxIdempotencyKeyLength = 255
	// There is a single Premium plan, so its checkouts have nothing to tell apart.
	premiumFingerprint = "premium"
)

type BillingHandler struct {
	points    PointsUseCases
	premium   PremiumUseCases
	checkouts CheckoutRunner
	log       *slog.Logger
}

func NewBillingHandler(points PointsUseCases, premium PremiumUseCases, checkouts CheckoutRunner, logger *slog.Logger) *BillingHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &BillingHandler{points: points, premium: premium, checkouts: checkouts, log: logger}
}

// Register mounts the user routes behind auth.
func (h *BillingHandler) Register(r gin.IRouter, auth gin.HandlerFunc) {
	g := r.Group("/v1/billing", auth)
	g.GET("/points/packages", h.listPackages)
	g.POST("/points/checkout", h.startPointsCheckout)
	g.GET("/points", h.getPoints)
	g.GET("/purchases/:id", h.getPurchase)
	g.GET("/premium", h.getPremium)
	g.POST("/premium/checkout", h.startPremiumCheckout)
	g.POST("/premium/cancel", h.cancelPremium)
}

type packageResponse struct {
	ID         string `json:"id"`
	Points     int    `json:"points"`
	PriceCents int64  `json:"price_cents"`
	Title      string `json:"title"`
}

func (h *BillingHandler) listPackages(c *gin.Context) {
	pkgs := h.points.Packages()
	out := make([]packageResponse, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, packageResponse{ID: p.ID, Points: p.Points, PriceCents: p.AmountCents, Title: p.Title})
	}
	c.JSON(http.StatusOK, gin.H{"packages": out})
}

type pointsCheckoutRequest struct {
	PackageID  string `json:"package_id" binding:"required"`
	PayerEmail string `json:"payer_email" binding:"omitempty,email"`
	ReturnTo   string `json:"return_to" binding:"omitempty,oneof=web app"`
}

func returnTarget(returnTo string) service.ReturnTarget {
	if returnTo == string(service.ReturnToApp) {
		return service.ReturnToApp
	}
	return service.ReturnToWeb
}

func checkoutFingerprint(base string, target service.ReturnTarget) string {
	if target == service.ReturnToApp {
		return base + "|app"
	}
	return base
}

type checkoutResponse struct {
	PurchaseID  *uuid.UUID `json:"purchase_id,omitempty"`
	CheckoutURL string     `json:"checkout_url"`
}

func (h *BillingHandler) startPointsCheckout(c *gin.Context) {
	var req pointsCheckoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, CodeInvalidRequest, invalidRequestMessage)
		return
	}
	key, ok := idempotencyKey(c)
	if !ok {
		return
	}
	userID := middleware.UserID(c)
	target := returnTarget(req.ReturnTo)
	checkout, err := h.checkouts.Run(c.Request.Context(),
		model.CheckoutKey{UserID: userID, Scope: model.CheckoutScopePoints, Key: key}, checkoutFingerprint(req.PackageID, target),
		func(ctx context.Context) (service.Checkout, error) {
			return h.points.StartCheckout(ctx, userID, req.PackageID, service.CheckoutPayer{Email: req.PayerEmail}, target)
		})
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	c.JSON(http.StatusCreated, checkoutResponse{PurchaseID: &checkout.PurchaseID, CheckoutURL: checkout.CheckoutURL})
}

type purchaseResponse struct {
	ID                uuid.UUID `json:"id"`
	PackageID         string    `json:"package_id"`
	Points            int       `json:"points"`
	PriceCents        int64     `json:"price_cents"`
	Status            string    `json:"status"`
	StatusDetail      string    `json:"status_detail"`
	PointsUnrecovered int       `json:"points_unrecovered"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func toPurchaseResponse(p model.PointPurchase) purchaseResponse {
	return purchaseResponse{
		ID: p.ID, PackageID: p.PackageID, Points: p.Points, PriceCents: p.AmountCents,
		Status: string(p.Status), StatusDetail: p.StatusDetail, PointsUnrecovered: p.PointsUnrecovered,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

type ledgerEntryResponse struct {
	Delta     int64     `json:"delta"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

type pointsResponse struct {
	Balance   int64                 `json:"balance"`
	Ledger    []ledgerEntryResponse `json:"ledger"`
	Purchases []purchaseResponse    `json:"purchases"`
}

// getPoints returns the balance with the latest movements and purchases;
// ?limit applies to both lists.
func (h *BillingHandler) getPoints(c *gin.Context) {
	limit, ok := parseLimit(c)
	if !ok {
		return
	}
	ctx, userID := c.Request.Context(), middleware.UserID(c)
	balance, err := h.points.Balance(ctx, userID)
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	entries, err := h.points.ListLedger(ctx, userID, limit)
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	purchases, err := h.points.ListPurchases(ctx, userID, limit)
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}

	resp := pointsResponse{
		Balance:   balance,
		Ledger:    make([]ledgerEntryResponse, 0, len(entries)),
		Purchases: make([]purchaseResponse, 0, len(purchases)),
	}
	for _, e := range entries {
		resp.Ledger = append(resp.Ledger, ledgerEntryResponse{Delta: e.Delta, Reason: string(e.Reason), CreatedAt: e.CreatedAt})
	}
	for _, p := range purchases {
		resp.Purchases = append(resp.Purchases, toPurchaseResponse(p))
	}
	c.JSON(http.StatusOK, resp)
}

// parseLimit reads ?limit; 0 (absent) lets the service use its default.
func parseLimit(c *gin.Context) (int, bool) {
	raw := c.Query("limit")
	if raw == "" {
		return 0, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 {
		writeError(c, http.StatusBadRequest, CodeInvalidRequest, invalidRequestMessage)
		return 0, false
	}
	return limit, true
}

func (h *BillingHandler) getPurchase(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		writeError(c, http.StatusNotFound, CodePurchaseNotFound, purchaseNotFoundMessage)
		return
	}
	p, err := h.points.GetPurchase(c.Request.Context(), middleware.UserID(c), id)
	if errors.Is(err, model.ErrNotFound) {
		writeError(c, http.StatusNotFound, CodePurchaseNotFound, purchaseNotFoundMessage)
		return
	}
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	c.JSON(http.StatusOK, toPurchaseResponse(p))
}

type premiumResponse struct {
	State        service.PremiumState `json:"state"`
	Active       bool                 `json:"active"`
	PremiumUntil *time.Time           `json:"premium_until"`
	Renews       bool                 `json:"renews"`
	MPStatus     string               `json:"mp_status"`
}

func toPremiumResponse(v service.PremiumView) premiumResponse {
	return premiumResponse{
		State:        v.State,
		Active:       v.State == service.PremiumStateActive || v.State == service.PremiumStateActiveCanceled,
		PremiumUntil: v.PremiumUntil,
		Renews:       v.Renews,
		MPStatus:     v.MPStatus,
	}
}

// getPremium with ?refresh=1 asks MP first; the return page uses it so
// Premium shows up even before the webhook arrives.
func (h *BillingHandler) getPremium(c *gin.Context) {
	refresh := c.Query("refresh") == "1" || c.Query("refresh") == "true"
	view, err := h.premium.State(c.Request.Context(), middleware.UserID(c), refresh)
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	c.JSON(http.StatusOK, toPremiumResponse(view))
}

type premiumCheckoutRequest struct {
	PayerEmail string `json:"payer_email" binding:"omitempty,email"`
	ReturnTo   string `json:"return_to" binding:"omitempty,oneof=web app"`
}

func (h *BillingHandler) startPremiumCheckout(c *gin.Context) {
	var req premiumCheckoutRequest
	// The body is optional: in sandbox the test buyer's email replaces it.
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, CodeInvalidRequest, invalidRequestMessage)
			return
		}
	}
	key, ok := idempotencyKey(c)
	if !ok {
		return
	}
	userID := middleware.UserID(c)
	target := returnTarget(req.ReturnTo)
	checkout, err := h.checkouts.Run(c.Request.Context(),
		model.CheckoutKey{UserID: userID, Scope: model.CheckoutScopePremium, Key: key}, checkoutFingerprint(premiumFingerprint, target),
		func(ctx context.Context) (service.Checkout, error) {
			url, err := h.premium.StartCheckout(ctx, userID, req.PayerEmail, target)
			return service.Checkout{CheckoutURL: url}, err
		})
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	c.JSON(http.StatusCreated, checkoutResponse{CheckoutURL: checkout.CheckoutURL})
}

// idempotencyKey reads the optional header. It answers the request itself if
// the key is too long.
func idempotencyKey(c *gin.Context) (string, bool) {
	key := c.GetHeader(idempotencyKeyHeader)
	if len(key) > maxIdempotencyKeyLength {
		writeError(c, http.StatusBadRequest, CodeInvalidRequest, invalidRequestMessage)
		return "", false
	}
	return key, true
}

func (h *BillingHandler) cancelPremium(c *gin.Context) {
	view, err := h.premium.Cancel(c.Request.Context(), middleware.UserID(c))
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	c.JSON(http.StatusOK, toPremiumResponse(view))
}
