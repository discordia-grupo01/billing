package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/service"
)

type fakeSimulator struct {
	userID   uuid.UUID
	approved *bool
	expired  bool
	view     service.PremiumView
	err      error
}

func (f *fakeSimulator) SimulateCharge(_ context.Context, userID uuid.UUID, approved bool) (service.PremiumView, error) {
	f.userID, f.approved = userID, &approved
	return f.view, f.err
}

func (f *fakeSimulator) SimulateExpiry(_ context.Context, userID uuid.UUID) (service.PremiumView, error) {
	f.userID, f.expired = userID, true
	return f.view, f.err
}

func newDevRouter(sim *fakeSimulator) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewDevHandler(sim, quietLog).Register(r)
	return r
}

func TestSimulateCharge(t *testing.T) {
	for outcome, want := range map[string]bool{"approved": true, "rejected": false} {
		t.Run(outcome, func(t *testing.T) {
			sim := &fakeSimulator{view: service.PremiumView{State: service.PremiumStateActive}}
			w := do(newDevRouter(sim), http.MethodPost, "/dev/billing/simulate-charge",
				`{"user_id":"`+userID.String()+`","outcome":"`+outcome+`"}`)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", w.Code, w.Body)
			}
			if sim.userID != userID || sim.approved == nil || *sim.approved != want {
				t.Errorf("called with %s approved=%v", sim.userID, sim.approved)
			}
			if got := decode[premiumResponse](t, w); got.State != service.PremiumStateActive || !got.Active {
				t.Errorf("unexpected response: %s", w.Body)
			}
		})
	}
}

func TestSimulateChargeInvalid(t *testing.T) {
	for name, body := range map[string]string{
		"no user":         `{"outcome":"approved"}`,
		"unknown outcome": `{"user_id":"` + userID.String() + `","outcome":"maybe"}`,
		"user not uuid":   `{"user_id":"x","outcome":"approved"}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := do(newDevRouter(&fakeSimulator{}), http.MethodPost, "/dev/billing/simulate-charge", body)
			assertError(t, w, http.StatusBadRequest, CodeInvalidRequest)
		})
	}
}

func TestSimulateChargeWithoutSubscription(t *testing.T) {
	w := do(newDevRouter(&fakeSimulator{err: service.ErrNoLiveSubscription}), http.MethodPost,
		"/dev/billing/simulate-charge", `{"user_id":"`+userID.String()+`","outcome":"approved"}`)
	assertError(t, w, http.StatusConflict, CodeNoLiveSubscription)
}

func TestSimulateExpiry(t *testing.T) {
	sim := &fakeSimulator{view: service.PremiumView{State: service.PremiumStateExpired}}
	w := do(newDevRouter(sim), http.MethodPost, "/dev/billing/simulate-expiry", `{"user_id":"`+userID.String()+`"}`)

	if w.Code != http.StatusOK || !sim.expired || sim.userID != userID {
		t.Fatalf("status %d, expired %v, user %s", w.Code, sim.expired, sim.userID)
	}
	if got := decode[premiumResponse](t, w); got.State != service.PremiumStateExpired || got.Active {
		t.Errorf("unexpected response: %s", w.Body)
	}
}

func TestSimulateExpiryErrors(t *testing.T) {
	assertError(t, do(newDevRouter(&fakeSimulator{}), http.MethodPost, "/dev/billing/simulate-expiry", `{}`),
		http.StatusBadRequest, CodeInvalidRequest)
	assertError(t, do(newDevRouter(&fakeSimulator{err: service.ErrNoActivePremium}), http.MethodPost,
		"/dev/billing/simulate-expiry", `{"user_id":"`+userID.String()+`"}`), http.StatusConflict, CodeNoActivePremium)
	assertError(t, do(newDevRouter(&fakeSimulator{err: errBoom}), http.MethodPost,
		"/dev/billing/simulate-expiry", `{"user_id":"`+userID.String()+`"}`), http.StatusInternalServerError, CodeInternalError)
}

func TestNewDevHandlerDefaultsLogger(t *testing.T) {
	if h := NewDevHandler(&fakeSimulator{}, nil); h.log == nil {
		t.Error("logger should default to slog.Default")
	}
}
