// Package apierr arma el formato de error común de la API
// ({"error": {"code", "message", "details"}}). Es el único lugar que conoce el
// sobre: handlers, middlewares y la recovery responden siempre por acá.
package apierr

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Códigos genéricos del catálogo común de Discordia.
const (
	CodeInvalidBody      = "INVALID_BODY"
	CodeInvalidQuery     = "INVALID_QUERY"
	CodeUnauthorized     = "UNAUTHORIZED"
	CodeForbidden        = "FORBIDDEN"
	CodeNotFound         = "NOT_FOUND"
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	CodePayloadTooLarge  = "PAYLOAD_TOO_LARGE"
	CodeInternal         = "INTERNAL_ERROR"
)

// Body es el error en sí. details nunca se omite: sin datos va como {}.
type Body struct {
	Code    string         `json:"code" example:"INVALID_BODY"`
	Message string         `json:"message" example:"La solicitud no es válida"`
	Details map[string]any `json:"details"`
} // @name ApiError

// ErrorResponse es el sobre de toda respuesta de error.
type ErrorResponse struct {
	Error Body `json:"error"`
} // @name ErrorResponse

// Abort corta la request con el error indicado. details nil se serializa como {}.
func Abort(c *gin.Context, status int, code, message string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	c.AbortWithStatusJSON(status, ErrorResponse{Error: Body{Code: code, Message: message, Details: details}})
}

// InvalidBody responde 400 INVALID_BODY; details va vacío si no se conoce el campo.
func InvalidBody(c *gin.Context, message string, details map[string]any) {
	Abort(c, http.StatusBadRequest, CodeInvalidBody, message, details)
}

// InvalidQuery responde 400 INVALID_QUERY indicando qué parámetro falló.
func InvalidQuery(c *gin.Context, param, message string) {
	Abort(c, http.StatusBadRequest, CodeInvalidQuery, message, map[string]any{"param": param})
}

func Unauthorized(c *gin.Context) {
	Abort(c, http.StatusUnauthorized, CodeUnauthorized, "La sesión expiró o ya no es válida", nil)
}

func Forbidden(c *gin.Context) {
	Abort(c, http.StatusForbidden, CodeForbidden, "No tenés permisos para realizar esta acción", nil)
}

func NotFound(c *gin.Context) {
	Abort(c, http.StatusNotFound, CodeNotFound, "El recurso solicitado no existe", nil)
}

func MethodNotAllowed(c *gin.Context) {
	Abort(c, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "El método no está permitido para este recurso", nil)
}

// PayloadTooLarge responde 413; maxBytes > 0 se informa en details.
func PayloadTooLarge(c *gin.Context, message string, maxBytes int64) {
	details := map[string]any{}
	if maxBytes > 0 {
		details["max_bytes"] = maxBytes
	}
	Abort(c, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, message, details)
}

func Internal(c *gin.Context) {
	Abort(c, http.StatusInternalServerError, CodeInternal, "Error interno del servidor", nil)
}

// RegisterFallbacks hace que la ruta inexistente y el método no permitido
// respondan con el sobre en vez del texto plano de Gin.
func RegisterFallbacks(r *gin.Engine) {
	r.HandleMethodNotAllowed = true
	r.NoRoute(NotFound)
	r.NoMethod(MethodNotAllowed)
}
