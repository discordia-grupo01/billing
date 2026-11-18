package handler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/apierr"
	"github.com/discordia-grupo01/billing/internal/model"
	"github.com/discordia-grupo01/billing/internal/service"
)

type fakeBoosts struct {
	boost     model.Boost
	page      model.BoostPage
	servers   model.BoostedServerPage
	err       error
	gotUser   uuid.UUID
	gotServer uuid.UUID
	gotKey    string
	gotID     uuid.UUID
	gotParams model.BoostListParams
	gotLimit  int
	gotOffset int
	calls     int
}

func (f *fakeBoosts) Request(_ context.Context, user, server uuid.UUID, key string) (model.Boost, error) {
	f.calls++
	f.gotUser, f.gotServer, f.gotKey = user, server, key
	return f.boost, f.err
}
func (f *fakeBoosts) Get(_ context.Context, user, id uuid.UUID) (model.Boost, error) {
	f.calls++
	f.gotUser, f.gotID = user, id
	return f.boost, f.err
}
func (f *fakeBoosts) List(_ context.Context, user uuid.UUID, params model.BoostListParams) (model.BoostPage, error) {
	f.calls++
	f.gotUser, f.gotParams = user, params
	return f.page, f.err
}
func (f *fakeBoosts) ListServers(_ context.Context, user uuid.UUID, limit, offset int) (model.BoostedServerPage, error) {
	f.calls++
	f.gotUser, f.gotLimit, f.gotOffset = user, limit, offset
	return f.servers, f.err
}
func (f *fakeBoosts) Remove(_ context.Context, user, id uuid.UUID) (model.Boost, error) {
	f.calls++
	f.gotUser, f.gotID = user, id
	return f.boost, f.err
}

func boostRouter(f *fakeBoosts) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewBoostHandler(f, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(r, fakeAuth)
	return r
}

func boostDo(r http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestBoostRequestAnswersAcceptedWithPending(t *testing.T) {
	server := uuid.New()
	f := &fakeBoosts{boost: model.Boost{ID: uuid.New(), ServerID: server, Points: 100, Status: model.BoostStatusPending}}

	w := boostDo(boostRouter(f), http.MethodPost, "/v1/billing/boosts", `{"server_id":"`+server.String()+`"}`,
		map[string]string{idempotencyKeyHeader: "click-1"})

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	if f.gotUser != userID || f.gotServer != server || f.gotKey != "click-1" {
		t.Errorf("user %s, server %s, key %q", f.gotUser, f.gotServer, f.gotKey)
	}
	var got boostResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" || got.Points != 100 || got.ServerID != server {
		t.Errorf("got %+v", got)
	}
}

func TestBoostRequestRejectsBadInputBeforeTheService(t *testing.T) {
	long := strings.Repeat("k", maxIdempotencyKeyLength+1)
	cases := map[string]struct {
		body    string
		headers map[string]string
	}{
		"not json":   {`nope`, nil},
		"no server":  {`{}`, nil},
		"bad server": {`{"server_id":"nope"}`, nil},
		"long key":   {`{"server_id":"` + uuid.NewString() + `"}`, map[string]string{idempotencyKeyHeader: long}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeBoosts{}
			w := boostDo(boostRouter(f), http.MethodPost, "/v1/billing/boosts", tc.body, tc.headers)
			if f.calls != 0 {
				t.Errorf("calls = %d", f.calls)
			}
			assertError(t, w, http.StatusBadRequest, apierr.CodeInvalidBody)
		})
	}
}

func TestBoostRequestInsufficientPointsSaysHowManyAreMissing(t *testing.T) {
	f := &fakeBoosts{err: model.InsufficientPointsError{Balance: 30, Cost: 100}}

	w := boostDo(boostRouter(f), http.MethodPost, "/v1/billing/boosts", `{"server_id":"`+uuid.NewString()+`"}`, nil)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d", w.Code)
	}
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != CodeInsufficientPoints || !strings.Contains(body.Error.Message, "70") {
		t.Errorf("error = %+v", body.Error)
	}
	if body.Error.Details["missing"] != float64(70) || body.Error.Details["balance"] != float64(30) || body.Error.Details["cost"] != float64(100) {
		t.Errorf("details = %v", body.Error.Details)
	}
}

func TestBoostRequestKeyReused(t *testing.T) {
	f := &fakeBoosts{err: service.ErrCheckoutKeyReused}
	w := boostDo(boostRouter(f), http.MethodPost, "/v1/billing/boosts", `{"server_id":"`+uuid.NewString()+`"}`, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d", w.Code)
	}
}

func TestBoostList(t *testing.T) {
	serverID := uuid.New()
	f := &fakeBoosts{page: model.BoostPage{
		Boosts: []model.Boost{{ID: uuid.New(), Status: model.BoostStatusRemoved}},
		Total:  13, Active: 7, Limit: 10, Offset: 10,
	}}

	w := boostDo(boostRouter(f), http.MethodGet, "/v1/billing/boosts?limit=10&offset=10&server_id="+serverID.String(), "", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	got := f.gotParams
	if got.Limit != 10 || got.Offset != 10 || got.ServerID == nil || *got.ServerID != serverID || f.gotUser != userID {
		t.Errorf("params = %+v", got)
	}
	var body boostPageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Boosts) != 1 || body.Boosts[0].Status != "removed" || body.Total != 13 || body.Active != 7 ||
		body.Limit != 10 || body.Offset != 10 {
		t.Errorf("got %+v", body)
	}
}

func TestBoostListWithoutFilters(t *testing.T) {
	f := &fakeBoosts{}
	w := boostDo(boostRouter(f), http.MethodGet, "/v1/billing/boosts", "", nil)
	if w.Code != http.StatusOK || f.gotParams != (model.BoostListParams{}) {
		t.Errorf("status = %d, params = %+v", w.Code, f.gotParams)
	}
	if !strings.Contains(w.Body.String(), `"boosts":[]`) {
		t.Errorf("body = %s", w.Body)
	}
}

func TestBoostListBadQueryAndFailure(t *testing.T) {
	for _, query := range []string{"limit=0", "offset=-1", "offset=x", "server_id=nope"} {
		f := &fakeBoosts{}
		w := boostDo(boostRouter(f), http.MethodGet, "/v1/billing/boosts?"+query, "", nil)
		assertError(t, w, http.StatusBadRequest, apierr.CodeInvalidQuery)
		param := strings.SplitN(query, "=", 2)[0]
		if got := decode[apierr.ErrorResponse](t, w).Error.Details["param"]; got != param {
			t.Errorf("%s: details.param = %v, want %s", query, got, param)
		}
		if f.calls != 0 {
			t.Errorf("%s: calls = %d", query, f.calls)
		}
	}
	if w := boostDo(boostRouter(&fakeBoosts{err: errBoom}), http.MethodGet, "/v1/billing/boosts", "", nil); w.Code != http.StatusInternalServerError {
		t.Errorf("failure = %d", w.Code)
	}
}

func TestBoostListServers(t *testing.T) {
	serverID := uuid.New()
	last := time.Date(2026, 11, 14, 18, 0, 0, 0, time.UTC)
	f := &fakeBoosts{servers: model.BoostedServerPage{
		Servers: []model.BoostedServer{{ServerID: serverID, Active: 7, Failed: 1, Removed: 2, LastBoostAt: last}},
		Total:   14, Limit: 10, Offset: 0,
	}}

	w := boostDo(boostRouter(f), http.MethodGet, "/v1/billing/boosts/servers?limit=10", "", nil)

	if w.Code != http.StatusOK || f.gotLimit != 10 || f.gotOffset != 0 || f.gotUser != userID {
		t.Fatalf("status = %d, limit = %d, offset = %d", w.Code, f.gotLimit, f.gotOffset)
	}
	var body boostedServerPageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := boostedServerResponse{ServerID: serverID, Active: 7, Failed: 1, Removed: 2, LastBoostAt: last}
	if len(body.Servers) != 1 || body.Servers[0] != want || body.Total != 14 || body.Limit != 10 {
		t.Errorf("got %+v", body)
	}
}

func TestBoostListServersEmptyBadQueryAndFailure(t *testing.T) {
	if w := boostDo(boostRouter(&fakeBoosts{}), http.MethodGet, "/v1/billing/boosts/servers", "", nil); !strings.Contains(w.Body.String(), `"servers":[]`) {
		t.Errorf("empty body = %s", w.Body)
	}
	for _, query := range []string{"limit=0", "offset=-1"} {
		if w := boostDo(boostRouter(&fakeBoosts{}), http.MethodGet, "/v1/billing/boosts/servers?"+query, "", nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d", query, w.Code)
		}
	}
	if w := boostDo(boostRouter(&fakeBoosts{err: errBoom}), http.MethodGet, "/v1/billing/boosts/servers", "", nil); w.Code != http.StatusInternalServerError {
		t.Errorf("failure = %d", w.Code)
	}
}

func TestBoostGetAndRemove(t *testing.T) {
	id := uuid.New()
	for method, wantStatus := range map[string]string{http.MethodGet: "pending", http.MethodDelete: "removed"} {
		f := &fakeBoosts{boost: model.Boost{ID: id, Status: model.BoostStatus(wantStatus)}}
		w := boostDo(boostRouter(f), method, "/v1/billing/boosts/"+id.String(), "", nil)
		if w.Code != http.StatusOK || f.gotID != id || f.gotUser != userID {
			t.Errorf("%s: status = %d, id = %s", method, w.Code, f.gotID)
		}
		if !strings.Contains(w.Body.String(), `"status":"`+wantStatus+`"`) {
			t.Errorf("%s: body = %s", method, w.Body)
		}
	}
}

func TestBoostGetAndRemoveErrors(t *testing.T) {
	cases := map[string]struct {
		path string
		err  error
		code int
		want string
	}{
		"bad id":     {"/v1/billing/boosts/nope", nil, http.StatusNotFound, CodeBoostNotFound},
		"not found":  {"/v1/billing/boosts/" + uuid.NewString(), model.ErrNotFound, http.StatusNotFound, CodeBoostNotFound},
		"not active": {"/v1/billing/boosts/" + uuid.NewString(), model.ErrBoostNotActive, http.StatusConflict, CodeBoostNotActive},
		"failure":    {"/v1/billing/boosts/" + uuid.NewString(), errBoom, http.StatusInternalServerError, apierr.CodeInternal},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := boostDo(boostRouter(&fakeBoosts{err: tc.err}), http.MethodDelete, tc.path, "", nil)
			if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("status = %d, body %s", w.Code, w.Body)
			}
		})
	}
}
