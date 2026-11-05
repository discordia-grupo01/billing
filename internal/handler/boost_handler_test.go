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

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/model"
	"github.com/discordia-grupo01/billing/internal/service"
)

type fakeBoosts struct {
	boost     model.Boost
	list      []model.Boost
	err       error
	gotUser   uuid.UUID
	gotServer uuid.UUID
	gotKey    string
	gotID     uuid.UUID
	gotLimit  int
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
func (f *fakeBoosts) List(_ context.Context, user uuid.UUID, limit int) ([]model.Boost, error) {
	f.calls++
	f.gotUser, f.gotLimit = user, limit
	return f.list, f.err
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
			if w.Code != http.StatusBadRequest || f.calls != 0 {
				t.Errorf("status = %d, calls = %d", w.Code, f.calls)
			}
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
	f := &fakeBoosts{list: []model.Boost{{ID: uuid.New(), Status: model.BoostStatusActive}}}

	w := boostDo(boostRouter(f), http.MethodGet, "/v1/billing/boosts?limit=5", "", nil)

	if w.Code != http.StatusOK || f.gotLimit != 5 {
		t.Fatalf("status = %d, limit = %d", w.Code, f.gotLimit)
	}
	var body struct {
		Boosts []boostResponse `json:"boosts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Boosts) != 1 || body.Boosts[0].Status != "active" {
		t.Errorf("got %+v", body)
	}
}

func TestBoostListEmptyIsAnArray(t *testing.T) {
	w := boostDo(boostRouter(&fakeBoosts{}), http.MethodGet, "/v1/billing/boosts", "", nil)
	if !strings.Contains(w.Body.String(), `"boosts":[]`) {
		t.Errorf("body = %s", w.Body)
	}
}

func TestBoostListBadLimitAndFailure(t *testing.T) {
	if w := boostDo(boostRouter(&fakeBoosts{}), http.MethodGet, "/v1/billing/boosts?limit=0", "", nil); w.Code != http.StatusBadRequest {
		t.Errorf("bad limit = %d", w.Code)
	}
	if w := boostDo(boostRouter(&fakeBoosts{err: errBoom}), http.MethodGet, "/v1/billing/boosts", "", nil); w.Code != http.StatusInternalServerError {
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
		"failure":    {"/v1/billing/boosts/" + uuid.NewString(), errBoom, http.StatusInternalServerError, CodeInternalError},
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
