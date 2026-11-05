package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/discordia-grupo01/billing/internal/model"
	"github.com/discordia-grupo01/billing/internal/service"
)

// Error codes clients can rely on; messages are for humans and may change.
const (
	CodeInvalidRequest       = "INVALID_REQUEST"
	CodeUnknownPackage       = "UNKNOWN_PACKAGE"
	CodePurchaseNotFound     = "PURCHASE_NOT_FOUND"
	CodePayerEmailRequired   = "PAYER_EMAIL_REQUIRED"
	CodeAlreadySubscribed    = "ALREADY_SUBSCRIBED"
	CodeNoLiveSubscription   = "NO_LIVE_SUBSCRIPTION"
	CodeNoActivePremium      = "NO_ACTIVE_PREMIUM"
	CodeInternalError        = "INTERNAL_ERROR"
	CodeInvalidSignature     = "INVALID_SIGNATURE"
	invalidRequestMessage    = "La solicitud no es válida"
	internalErrorMessage     = "No pudimos completar la operación, probá de nuevo en unos minutos"
	invalidSignatureMessage  = "Firma inválida"
	purchaseNotFoundMessage  = "La compra no existe"
	unknownPackageMessage    = "El paquete de puntos no existe"
	payerEmailMessage        = "Falta el email del pagador"
	alreadySubscribedMessage = "Ya tenés una suscripción a Premium activa"
	noSubscriptionMessage    = "No tenés una suscripción a Premium activa"
	noActivePremiumMessage   = "No tenés Premium activo"

	CodeCheckoutInProgress    = "CHECKOUT_IN_PROGRESS"
	CodeIdempotencyKeyReused  = "IDEMPOTENCY_KEY_REUSED"
	checkoutInProgressMessage = "Ya estamos procesando este pedido, probá de nuevo en unos segundos"
	checkoutKeyReusedMessage  = "Esta clave ya se usó para otro pedido"

	CodeBoostNotFound        = "BOOST_NOT_FOUND"
	CodeBoostNotActive       = "BOOST_NOT_ACTIVE"
	CodeInsufficientPoints   = "INSUFFICIENT_POINTS"
	boostNotFoundMessage     = "El boost no existe"
	boostNotActiveMessage    = "Solo se puede retirar un boost activo"
	insufficientPointsFormat = "Te faltan %d puntos para boostear este servidor"
)

type errorBody struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func writeError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, errorBody{Error: apiError{Code: code, Message: message}})
}

// writeServiceError maps the use cases' errors to HTTP. Anything unknown is
// logged and hidden behind a 500.
func writeServiceError(c *gin.Context, log *slog.Logger, err error) {
	var insufficient model.InsufficientPointsError
	if errors.As(err, &insufficient) {
		c.AbortWithStatusJSON(http.StatusConflict, errorBody{Error: apiError{
			Code:    CodeInsufficientPoints,
			Message: fmt.Sprintf(insufficientPointsFormat, insufficient.Missing()),
			Details: map[string]any{
				"balance": insufficient.Balance,
				"cost":    insufficient.Cost,
				"missing": insufficient.Missing(),
			},
		}})
		return
	}
	switch {
	case errors.Is(err, model.ErrBoostNotActive):
		writeError(c, http.StatusConflict, CodeBoostNotActive, boostNotActiveMessage)
	case errors.Is(err, service.ErrUnknownPackage):
		writeError(c, http.StatusBadRequest, CodeUnknownPackage, unknownPackageMessage)
	case errors.Is(err, service.ErrPayerEmailRequired):
		writeError(c, http.StatusBadRequest, CodePayerEmailRequired, payerEmailMessage)
	case errors.Is(err, service.ErrAlreadySubscribed):
		writeError(c, http.StatusConflict, CodeAlreadySubscribed, alreadySubscribedMessage)
	case errors.Is(err, service.ErrNoLiveSubscription):
		writeError(c, http.StatusConflict, CodeNoLiveSubscription, noSubscriptionMessage)
	case errors.Is(err, service.ErrNoActivePremium):
		writeError(c, http.StatusConflict, CodeNoActivePremium, noActivePremiumMessage)
	case errors.Is(err, service.ErrCheckoutInProgress):
		writeError(c, http.StatusConflict, CodeCheckoutInProgress, checkoutInProgressMessage)
	case errors.Is(err, service.ErrCheckoutKeyReused):
		writeError(c, http.StatusUnprocessableEntity, CodeIdempotencyKeyReused, checkoutKeyReusedMessage)
	default:
		log.Error("request failed", "method", c.Request.Method, "path", c.FullPath(), "error", err)
		writeError(c, http.StatusInternalServerError, CodeInternalError, internalErrorMessage)
	}
}
