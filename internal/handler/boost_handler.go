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
)

// BoostUseCases is what the boost routes need from service.BoostService.
type BoostUseCases interface {
	Request(ctx context.Context, userID, serverID uuid.UUID, idempotencyKey string) (model.Boost, error)
	Get(ctx context.Context, userID, id uuid.UUID) (model.Boost, error)
	List(ctx context.Context, userID uuid.UUID, params model.BoostListParams) (model.BoostPage, error)
	ListServers(ctx context.Context, userID uuid.UUID, limit, offset int) (model.BoostedServerPage, error)
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
	g.GET("/servers", h.listServers)
	g.GET("/:id", h.get)
	g.DELETE("/:id", h.remove)
}

type boostRequest struct {
	ServerID string `json:"server_id" binding:"required,uuid" format:"uuid"`
} // @name BoostRequest

type boostResponse struct {
	ID       uuid.UUID `json:"id" format:"uuid"`
	ServerID uuid.UUID `json:"server_id" format:"uuid"`
	Points   int       `json:"points" example:"100"`
	// failed gave the points back; removed did not.
	Status string `json:"status" enums:"pending,active,failed,removed"`
	// Why it failed or was removed: server_not_found, server_suspended,
	// not_a_member, max_level_reached, cancelled, no_response or
	// server_deleted. Empty if it doesn't apply, also when the owner removed it.
	StatusDetail string    `json:"status_detail"`
	CreatedAt    time.Time `json:"created_at" format:"date-time"`
	// When a boost was removed, when it was removed.
	UpdatedAt time.Time `json:"updated_at" format:"date-time"`
} // @name Boost

type boostPageResponse struct {
	Boosts []boostResponse `json:"boosts"`
	// Every boost that matches the filter, not only the ones in this page.
	Total int `json:"total" example:"13"`
	// Active boosts that match the filter, not only the ones in this page.
	Active int `json:"active" example:"7"`
	Limit  int `json:"limit" example:"10"`
	Offset int `json:"offset" example:"0"`
} // @name BoostPage

type boostedServerResponse struct {
	ServerID    uuid.UUID `json:"server_id" format:"uuid"`
	Active      int       `json:"active" example:"7"`
	Pending     int       `json:"pending" example:"0"`
	Failed      int       `json:"failed" example:"1"`
	Removed     int       `json:"removed" example:"2"`
	LastBoostAt time.Time `json:"last_boost_at" format:"date-time"`
} // @name BoostedServer

type boostedServerPageResponse struct {
	Servers []boostedServerResponse `json:"servers"`
	Total   int                     `json:"total" example:"14"`
	Limit   int                     `json:"limit" example:"10"`
	Offset  int                     `json:"offset" example:"0"`
} // @name BoostedServerPage

func toBoostResponse(b model.Boost) boostResponse {
	return boostResponse{
		ID: b.ID, ServerID: b.ServerID, Points: b.Points, Status: string(b.Status),
		StatusDetail: b.StatusDetail, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt,
	}
}

// request answers 202: the boost stays pending until servers decides, and the
// client polls GET /v1/billing/boosts/:id.
//
// @Summary      Boostea un servidor con 100 puntos
// @Description  Reserva los puntos y le pregunta a servers si puede aplicar el boost, así que responde 202 con el boost en `pending`. Hay que consultar `GET /v1/billing/boosts/{id}` hasta que deje de estar `pending`: normalmente tarda segundos. Si servers lo rechaza (servidor inexistente o suspendido, quien boostea no es miembro, o el servidor ya tiene los 30 boosts del último nivel) queda `failed` y los puntos vuelven. Si no responde en 24 horas, también vuelven. Con `Idempotency-Key`, un doble clic devuelve el mismo boost y cobra una sola vez.
// @Tags         Boosts
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        Idempotency-Key  header  string        false  "Clave única por intento, por ejemplo un UUID generado al abrir la pantalla. Repetir el pedido con la misma clave devuelve el mismo boost en vez de crear otro; se recuerda durante 24 horas."  maxlength(255)
// @Param        body             body    boostRequest  true   "Servidor a boostear"
// @Success      202  {object}  boostResponse  "Boost en pending, con los puntos ya descontados"
// @Failure      400  {object}  errorBody  "INVALID_REQUEST"
// @Failure      401  {object}  errorBody  "UNAUTHORIZED"
// @Failure      409  {object}  errorBody  "INSUFFICIENT_POINTS, con balance, cost y missing en details"
// @Failure      422  {object}  errorBody  "IDEMPOTENCY_KEY_REUSED, la Idempotency-Key ya se usó para otro servidor"
// @Failure      500  {object}  errorBody  "INTERNAL_ERROR"
// @Router       /v1/billing/boosts [post]
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

// list pages the user's boosts; ?server_id= keeps one server's.
//
// @Summary      Mis boosts, del más nuevo al más viejo, por páginas
// @Description  Todos los estados, también los retirados (`removed`). Con `server_id`, solo los de ese servidor. `total` y `active` cuentan todos los boosts del filtro, no solo los de la página.
// @Tags         Boosts
// @Produce      json
// @Security     BearerAuth
// @Param        server_id  query  string  false  "Solo los boosts de este servidor; vacío equivale a no enviarlo"  format(uuid)
// @Param        limit      query  int     false  "Tamaño de la página. Un valor mayor que 100 se trata como 100."  minimum(1)  default(50)
// @Param        offset     query  int     false  "Cuántos boosts saltear"  minimum(0)  default(0)
// @Success      200  {object}  boostPageResponse  "Página de boosts"
// @Failure      400  {object}  errorBody  "INVALID_REQUEST"
// @Failure      401  {object}  errorBody  "UNAUTHORIZED"
// @Failure      500  {object}  errorBody  "INTERNAL_ERROR"
// @Router       /v1/billing/boosts [get]
func (h *BoostHandler) list(c *gin.Context) {
	params, ok := parseBoostListParams(c)
	if !ok {
		return
	}
	page, err := h.boosts.List(c.Request.Context(), middleware.UserID(c), params)
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	out := boostPageResponse{
		Boosts: make([]boostResponse, 0, len(page.Boosts)),
		Total:  page.Total, Active: page.Active, Limit: page.Limit, Offset: page.Offset,
	}
	for _, b := range page.Boosts {
		out.Boosts = append(out.Boosts, toBoostResponse(b))
	}
	c.JSON(http.StatusOK, out)
}

// listServers pages the servers the user boosted, with their boosts by status.
//
// @Summary      Servidores que boosteé, por páginas
// @Description  Un servidor por fila, el boosteado más recientemente primero, con cuántos de mis boosts hay ahí en cada estado. Entra si tengo algún boost, en cualquier estado.
// @Tags         Boosts
// @Produce      json
// @Security     BearerAuth
// @Param        limit   query  int  false  "Tamaño de la página. Un valor mayor que 100 se trata como 100."  minimum(1)  default(50)
// @Param        offset  query  int  false  "Cuántos servidores saltear"  minimum(0)  default(0)
// @Success      200  {object}  boostedServerPageResponse  "Página de servidores"
// @Failure      400  {object}  errorBody  "INVALID_REQUEST"
// @Failure      401  {object}  errorBody  "UNAUTHORIZED"
// @Failure      500  {object}  errorBody  "INTERNAL_ERROR"
// @Router       /v1/billing/boosts/servers [get]
func (h *BoostHandler) listServers(c *gin.Context) {
	limit, ok := parseLimit(c)
	if !ok {
		return
	}
	offset, ok := parseOffset(c)
	if !ok {
		return
	}
	page, err := h.boosts.ListServers(c.Request.Context(), middleware.UserID(c), limit, offset)
	if err != nil {
		writeServiceError(c, h.log, err)
		return
	}
	out := boostedServerPageResponse{
		Servers: make([]boostedServerResponse, 0, len(page.Servers)),
		Total:   page.Total, Limit: page.Limit, Offset: page.Offset,
	}
	for _, s := range page.Servers {
		out.Servers = append(out.Servers, boostedServerResponse{
			ServerID: s.ServerID, Active: s.Active, Pending: s.Pending,
			Failed: s.Failed, Removed: s.Removed, LastBoostAt: s.LastBoostAt,
		})
	}
	c.JSON(http.StatusOK, out)
}

func parseBoostListParams(c *gin.Context) (model.BoostListParams, bool) {
	limit, ok := parseLimit(c)
	if !ok {
		return model.BoostListParams{}, false
	}
	offset, ok := parseOffset(c)
	if !ok {
		return model.BoostListParams{}, false
	}
	params := model.BoostListParams{Limit: limit, Offset: offset}
	if raw := c.Query("server_id"); raw != "" {
		serverID, err := uuid.Parse(raw)
		if err != nil {
			writeError(c, http.StatusBadRequest, CodeInvalidRequest, invalidRequestMessage)
			return model.BoostListParams{}, false
		}
		params.ServerID = &serverID
	}
	return params, true
}

// parseOffset reads ?offset; absent is 0.
func parseOffset(c *gin.Context) (int, bool) {
	raw := c.Query("offset")
	if raw == "" {
		return 0, true
	}
	offset, err := strconv.Atoi(raw)
	if err != nil || offset < 0 {
		writeError(c, http.StatusBadRequest, CodeInvalidRequest, invalidRequestMessage)
		return 0, false
	}
	return offset, true
}

// get answers the state of one of the user's boosts.
//
// @Summary      Estado de un boost propio
// @Tags         Boosts
// @Produce      json
// @Security     BearerAuth
// @Param        id  path  string  true  "ID del boost"  format(uuid)
// @Success      200  {object}  boostResponse  "El boost"
// @Failure      401  {object}  errorBody  "UNAUTHORIZED"
// @Failure      404  {object}  errorBody  "BOOST_NOT_FOUND; también si el boost es de otro usuario o el id no es un UUID"
// @Failure      500  {object}  errorBody  "INTERNAL_ERROR"
// @Router       /v1/billing/boosts/{id} [get]
func (h *BoostHandler) get(c *gin.Context) {
	h.respond(c, h.boosts.Get)
}

// remove takes back one of the user's active boosts.
//
// @Summary      Retira un boost activo propio, sin devolver los puntos
// @Description  Se puede retirar aunque ya no seas miembro del servidor.
// @Tags         Boosts
// @Produce      json
// @Security     BearerAuth
// @Param        id  path  string  true  "ID del boost"  format(uuid)
// @Success      200  {object}  boostResponse  "Boost en removed"
// @Failure      401  {object}  errorBody  "UNAUTHORIZED"
// @Failure      404  {object}  errorBody  "BOOST_NOT_FOUND; también si el boost es de otro usuario o el id no es un UUID"
// @Failure      409  {object}  errorBody  "BOOST_NOT_ACTIVE, solo se puede retirar un boost activo"
// @Failure      500  {object}  errorBody  "INTERNAL_ERROR"
// @Router       /v1/billing/boosts/{id} [delete]
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
