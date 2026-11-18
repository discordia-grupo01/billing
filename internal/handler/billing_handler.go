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
	ID         string `json:"id" example:"points_550"`
	Points     int    `json:"points" example:"550"`
	PriceCents int64  `json:"price_cents" example:"500000"`
	Title      string `json:"title" example:"550 puntos"`
} // @name Package

type packagesResponse struct {
	Packages []packageResponse `json:"packages"`
} // @name PackageList

// listPackages answers the points packages on sale.
//
// @Summary      Paquetes de puntos a la venta
// @Description  Los precios van en centavos de ARS.
// @Tags         Points
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  packagesResponse  "Paquetes disponibles"
// @Failure      401  {object}  errorBody  "UNAUTHORIZED"
// @Router       /v1/billing/points/packages [get]
func (h *BillingHandler) listPackages(c *gin.Context) {
	pkgs := h.points.Packages()
	out := make([]packageResponse, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, packageResponse{ID: p.ID, Points: p.Points, PriceCents: p.AmountCents, Title: p.Title})
	}
	c.JSON(http.StatusOK, gin.H{"packages": out})
}

type pointsCheckoutRequest struct {
	PackageID  string `json:"package_id" binding:"required" example:"points_550"`
	PayerEmail string `json:"payer_email" binding:"omitempty,email" validate:"optional" format:"email"` // optional: preloads the email in Mercado Pago
	// With "app", Mercado Pago returns to <CHECKOUT_RETURN_URL>/app-link/billing/return, which opens the mobile app.
	ReturnTo string `json:"return_to" binding:"omitempty,oneof=web app" validate:"optional" enums:"web,app" default:"web"`
} // @name PointsCheckoutRequest

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
	PurchaseID  *uuid.UUID `json:"purchase_id,omitempty" format:"uuid" validate:"optional"`
	CheckoutURL string     `json:"checkout_url" format:"uri"`
} // @name Checkout

// premiumCheckoutResponse is what POST /v1/billing/premium/checkout
// answers: Premium has no purchase, so there is no purchase_id.
type premiumCheckoutResponse struct {
	CheckoutURL string `json:"checkout_url" format:"uri"`
} // @name PremiumCheckout

// startPointsCheckout creates a points purchase and its Mercado Pago order.
//
// @Summary      Crea una compra de puntos y devuelve la página de pago de Mercado Pago
// @Description  Al terminar, Mercado Pago vuelve a `<CHECKOUT_RETURN_URL>/billing/return?kind=points&purchase_id=…`. La acreditación la hace el webhook; la pantalla de retorno consulta `GET /v1/billing/purchases/{id}` hasta que la compra deje de estar `pending`.
// @Tags         Points
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        Idempotency-Key  header  string                 false  "Clave única por intento de compra, por ejemplo un UUID generado al abrir la pantalla. Repetir el pedido con la misma clave devuelve el mismo checkout en vez de crear otro; se recuerda durante 24 horas."  maxlength(255)
// @Param        body             body    pointsCheckoutRequest  true   "Paquete a comprar"
// @Success      201  {object}  checkoutResponse  "Compra creada"
// @Failure      400  {object}  errorBody  "INVALID_REQUEST o UNKNOWN_PACKAGE"
// @Failure      401  {object}  errorBody  "UNAUTHORIZED"
// @Failure      409  {object}  errorBody  "CHECKOUT_IN_PROGRESS, otro pedido con la misma Idempotency-Key sigue en curso"
// @Failure      422  {object}  errorBody  "IDEMPOTENCY_KEY_REUSED, la Idempotency-Key ya se usó para otro pedido"
// @Failure      500  {object}  errorBody  "INTERNAL_ERROR"
// @Router       /v1/billing/points/checkout [post]
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
	ID         uuid.UUID `json:"id" format:"uuid"`
	PackageID  string    `json:"package_id" example:"points_550"`
	Points     int       `json:"points" example:"550"`
	PriceCents int64     `json:"price_cents" example:"500000"`
	Status     string    `json:"status" enums:"pending,paid,failed,expired,refunded,charged_back"`
	// Why the purchase ended as it did; empty if it doesn't apply.
	StatusDetail string `json:"status_detail"`
	// Points already spent that couldn't be taken back on a refund or chargeback.
	PointsUnrecovered int       `json:"points_unrecovered"`
	CreatedAt         time.Time `json:"created_at" format:"date-time"`
	UpdatedAt         time.Time `json:"updated_at" format:"date-time"`
} // @name Purchase

func toPurchaseResponse(p model.PointPurchase) purchaseResponse {
	return purchaseResponse{
		ID: p.ID, PackageID: p.PackageID, Points: p.Points, PriceCents: p.AmountCents,
		Status: string(p.Status), StatusDetail: p.StatusDetail, PointsUnrecovered: p.PointsUnrecovered,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

type ledgerEntryResponse struct {
	// Positive when points are credited, negative when they are taken back or spent.
	Delta     int64     `json:"delta"`
	Reason    string    `json:"reason" enums:"purchase,refund,chargeback,boost,boost_refund"`
	ServerID  *string   `json:"server_id,omitempty"`
	CreatedAt time.Time `json:"created_at" format:"date-time"`
} // @name LedgerEntry

type pointsResponse struct {
	Balance   int64                 `json:"balance"`
	Ledger    []ledgerEntryResponse `json:"ledger"`
	Purchases []purchaseResponse    `json:"purchases"`
} // @name Points

// getPoints returns the balance with the latest movements and purchases;
// ?limit applies to both lists.
//
// @Summary      Saldo, últimos movimientos y últimas compras
// @Tags         Points
// @Produce      json
// @Security     BearerAuth
// @Param        limit  query  int  false  "Cuántos movimientos y cuántas compras devolver; aplica a las dos listas. Un valor mayor que 100 se trata como 100."  minimum(1)  default(20)
// @Success      200  {object}  pointsResponse  "Saldo y listas"
// @Failure      400  {object}  errorBody  "INVALID_REQUEST"
// @Failure      401  {object}  errorBody  "UNAUTHORIZED"
// @Failure      500  {object}  errorBody  "INTERNAL_ERROR"
// @Router       /v1/billing/points [get]
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
		var serverID *string
		if e.ServerID != nil {
			s := e.ServerID.String()
			serverID = &s
		}
		resp.Ledger = append(resp.Ledger, ledgerEntryResponse{
			Delta:     e.Delta,
			Reason:    string(e.Reason),
			ServerID:  serverID,
			CreatedAt: e.CreatedAt,
		})
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

// getPurchase answers the state of one of the user's purchases.
//
// @Summary      Estado de una compra propia
// @Description  Lo consulta la pantalla de retorno del checkout hasta que la compra deja de estar `pending`.
// @Tags         Points
// @Produce      json
// @Security     BearerAuth
// @Param        id  path  string  true  "ID de la compra"  format(uuid)
// @Success      200  {object}  purchaseResponse  "La compra"
// @Failure      401  {object}  errorBody  "UNAUTHORIZED"
// @Failure      404  {object}  errorBody  "PURCHASE_NOT_FOUND; también si la compra es de otro usuario o el id no es un UUID"
// @Failure      500  {object}  errorBody  "INTERNAL_ERROR"
// @Router       /v1/billing/purchases/{id} [get]
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
	State  service.PremiumState `json:"state" enums:"none,processing_first_payment,active,active_canceled,expired"`
	Active bool                 `json:"active"`
	// Null if the user never paid.
	PremiumUntil *time.Time `json:"premium_until" extensions:"x-nullable" format:"date-time"`
	Renews       bool       `json:"renews"`
	// Status of the latest subscription in Mercado Pago; empty if there is none.
	MPStatus string `json:"mp_status"`
} // @name Premium

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
//
// @Summary      Estado de Premium del usuario
// @Tags         Premium
// @Produce      json
// @Security     BearerAuth
// @Param        refresh  query  string  false  "Con `1` o `true`, consulta a Mercado Pago antes de responder (usar al volver del checkout); cualquier otro valor se ignora"
// @Success      200  {object}  premiumResponse  "Estado de Premium"
// @Failure      401  {object}  errorBody  "UNAUTHORIZED"
// @Failure      500  {object}  errorBody  "INTERNAL_ERROR"
// @Router       /v1/billing/premium [get]
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
	// Required in production; in sandbox the test buyer's email replaces it.
	PayerEmail string `json:"payer_email" binding:"omitempty,email" validate:"optional" format:"email"`
	// With "app", Mercado Pago returns to <CHECKOUT_RETURN_URL>/app-link/billing/return, which opens the mobile app.
	ReturnTo string `json:"return_to" binding:"omitempty,oneof=web app" validate:"optional" enums:"web,app" default:"web"`
} // @name PremiumCheckoutRequest

// startPremiumCheckout creates the subscription in Mercado Pago.
//
// @Summary      Crea la suscripción y devuelve la página donde se carga la tarjeta
// @Description  Al terminar, Mercado Pago vuelve a `<CHECKOUT_RETURN_URL>/billing/return?kind=premium`. El cuerpo es opcional.
// @Tags         Premium
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        Idempotency-Key  header  string                 false  "Clave única por intento de compra, por ejemplo un UUID generado al abrir la pantalla. Repetir el pedido con la misma clave devuelve el mismo checkout en vez de crear otro; se recuerda durante 24 horas."  maxlength(255)
// @Param        body             body    premiumCheckoutRequest false  "Datos del pagador"
// @Success      201  {object}  premiumCheckoutResponse  "Suscripción creada"
// @Failure      400  {object}  errorBody  "INVALID_REQUEST o PAYER_EMAIL_REQUIRED"
// @Failure      401  {object}  errorBody  "UNAUTHORIZED"
// @Failure      409  {object}  errorBody  "ALREADY_SUBSCRIBED, ya tiene una suscripción que Mercado Pago sigue cobrando; o CHECKOUT_IN_PROGRESS, otro pedido con la misma Idempotency-Key sigue en curso"
// @Failure      422  {object}  errorBody  "IDEMPOTENCY_KEY_REUSED, la Idempotency-Key ya se usó para otro pedido"
// @Failure      500  {object}  errorBody  "INTERNAL_ERROR"
// @Router       /v1/billing/premium/checkout [post]
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
	c.JSON(http.StatusCreated, premiumCheckoutResponse{CheckoutURL: checkout.CheckoutURL})
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

// cancelPremium stops the renewal of the subscription.
//
// @Summary      Cancela la renovación; Premium sigue hasta premium_until
// @Tags         Premium
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  premiumResponse  "Premium sin renovación"
// @Failure      401  {object}  errorBody  "UNAUTHORIZED"
// @Failure      409  {object}  errorBody  "NO_LIVE_SUBSCRIPTION, no hay una suscripción que cancelar"
// @Failure      500  {object}  errorBody  "INTERNAL_ERROR"
// @Router       /v1/billing/premium/cancel [post]
func (h *BillingHandler) cancelPremium(c *gin.Context) {
	view, err := h.premium.Cancel(c.Request.Context(), middleware.UserID(c))
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	c.JSON(http.StatusOK, toPremiumResponse(view))
}
