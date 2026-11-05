package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/middleware"
	"github.com/discordia-grupo01/billing/internal/model"
	"github.com/discordia-grupo01/billing/internal/service"
)

var (
	userID     = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	purchaseID = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	errBoom    = errors.New("boom")
	createdAt  = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
)

type fakePoints struct {
	packageID string
	payer     service.CheckoutPayer
	checkout  service.Checkout
	purchase  model.PointPurchase
	balance   int64
	ledger    []model.LedgerEntry
	purchases []model.PointPurchase
	limit     int
	err       error
	failOn    string
}

func (f *fakePoints) fail(method string) error {
	if f.failOn == "" || f.failOn == method {
		return f.err
	}
	return nil
}

func (f *fakePoints) Packages() []service.PointsPackage {
	return []service.PointsPackage{{ID: "points_100", Points: 100, AmountCents: 1_000_00, Title: "100 puntos"}}
}

func (f *fakePoints) StartCheckout(_ context.Context, uid uuid.UUID, packageID string, payer service.CheckoutPayer) (service.Checkout, error) {
	if uid != userID {
		return service.Checkout{}, errors.New("wrong user")
	}
	f.packageID, f.payer = packageID, payer
	return f.checkout, f.fail("StartCheckout")
}

func (f *fakePoints) GetPurchase(_ context.Context, uid, id uuid.UUID) (model.PointPurchase, error) {
	if err := f.fail("GetPurchase"); err != nil {
		return model.PointPurchase{}, err
	}
	if uid != f.purchase.UserID || id != f.purchase.ID {
		return model.PointPurchase{}, model.ErrNotFound
	}
	return f.purchase, nil
}

func (f *fakePoints) Balance(context.Context, uuid.UUID) (int64, error) {
	return f.balance, f.fail("Balance")
}

func (f *fakePoints) ListPurchases(_ context.Context, _ uuid.UUID, limit int) ([]model.PointPurchase, error) {
	f.limit = limit
	return f.purchases, f.fail("ListPurchases")
}

func (f *fakePoints) ListLedger(_ context.Context, _ uuid.UUID, limit int) ([]model.LedgerEntry, error) {
	f.limit = limit
	return f.ledger, f.fail("ListLedger")
}

type fakePremium struct {
	payerEmail string
	refresh    bool
	initPoint  string
	view       service.PremiumView
	err        error
}

func (f *fakePremium) StartCheckout(_ context.Context, _ uuid.UUID, payerEmail string) (string, error) {
	f.payerEmail = payerEmail
	return f.initPoint, f.err
}

func (f *fakePremium) Cancel(context.Context, uuid.UUID) (service.PremiumView, error) {
	return f.view, f.err
}

func (f *fakePremium) State(_ context.Context, _ uuid.UUID, refresh bool) (service.PremiumView, error) {
	f.refresh = refresh
	return f.view, f.err
}

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeAuth stands in for middleware.Auth, which has its own tests.
func fakeAuth(c *gin.Context) {
	middleware.SetUserID(c, userID)
	c.Next()
}

func newRouter(points *fakePoints, premium *fakePremium) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewBillingHandler(points, premium, &fakeRunner{}, quietLog).Register(r, fakeAuth)
	return r
}

// fakeRunner runs the checkout unless err is set, and records what it was given.
type fakeRunner struct {
	key         model.CheckoutKey
	fingerprint string
	err         error
}

func (f *fakeRunner) Run(ctx context.Context, k model.CheckoutKey, fingerprint string, checkout func(context.Context) (service.Checkout, error)) (service.Checkout, error) {
	f.key, f.fingerprint = k, fingerprint
	if f.err != nil {
		return service.Checkout{}, f.err
	}
	return checkout(ctx)
}

func routerWithRunner(runner CheckoutRunner) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewBillingHandler(
		&fakePoints{checkout: service.Checkout{PurchaseID: purchaseID, CheckoutURL: "https://mp.test/c"}},
		&fakePremium{initPoint: "https://mp.test/sub"}, runner, quietLog,
	).Register(r, fakeAuth)
	return r
}

func doWithKey(r http.Handler, path, body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotencyKeyHeader, key)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func do(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid JSON %q: %v", w.Body.String(), err)
	}
	return v
}

func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", w.Code, status, w.Body)
	}
	if got := decode[errorBody](t, w).Error.Code; got != code {
		t.Errorf("code = %q, want %q", got, code)
	}
}

func TestListPackages(t *testing.T) {
	w := do(newRouter(&fakePoints{}, &fakePremium{}), http.MethodGet, "/v1/billing/points/packages", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	got := decode[struct{ Packages []packageResponse }](t, w).Packages
	want := packageResponse{ID: "points_100", Points: 100, PriceCents: 1_000_00, Title: "100 puntos"}
	if len(got) != 1 || got[0] != want {
		t.Errorf("packages = %+v, want [%+v]", got, want)
	}
}

func TestStartPointsCheckout(t *testing.T) {
	points := &fakePoints{checkout: service.Checkout{PurchaseID: purchaseID, CheckoutURL: "https://mp.test/c"}}
	w := do(newRouter(points, &fakePremium{}), http.MethodPost, "/v1/billing/points/checkout",
		`{"package_id":"points_100","payer_email":"ana@test.com"}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	got := decode[checkoutResponse](t, w)
	if got.PurchaseID == nil || *got.PurchaseID != purchaseID || got.CheckoutURL != "https://mp.test/c" {
		t.Errorf("unexpected response: %s", w.Body)
	}
	if points.packageID != "points_100" || points.payer.Email != "ana@test.com" {
		t.Errorf("unexpected call: %q %+v", points.packageID, points.payer)
	}
}

func TestStartPointsCheckoutInvalid(t *testing.T) {
	for name, body := range map[string]string{
		"no body":       "",
		"no package":    `{}`,
		"invalid email": `{"package_id":"points_100","payer_email":"nope"}`,
		"not JSON":      `{`,
	} {
		t.Run(name, func(t *testing.T) {
			w := do(newRouter(&fakePoints{}, &fakePremium{}), http.MethodPost, "/v1/billing/points/checkout", body)
			assertError(t, w, http.StatusBadRequest, CodeInvalidRequest)
		})
	}
}

func TestStartPointsCheckoutErrors(t *testing.T) {
	tests := map[string]struct {
		err    error
		status int
		code   string
	}{
		"unknown package": {service.ErrUnknownPackage, http.StatusBadRequest, CodeUnknownPackage},
		"internal":        {errBoom, http.StatusInternalServerError, CodeInternalError},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w := do(newRouter(&fakePoints{err: tc.err}, &fakePremium{}), http.MethodPost,
				"/v1/billing/points/checkout", `{"package_id":"x"}`)
			assertError(t, w, tc.status, tc.code)
		})
	}
}

func TestGetPoints(t *testing.T) {
	points := &fakePoints{
		balance: 650,
		ledger:  []model.LedgerEntry{{Delta: 550, Reason: model.LedgerReasonPurchase, RefID: "ORD1", CreatedAt: createdAt}},
		purchases: []model.PointPurchase{{
			ID: purchaseID, UserID: userID, PackageID: "points_550", Points: 550, AmountCents: 5_000_00,
			Status: model.PurchaseStatusPaid, StatusDetail: "accredited", CreatedAt: createdAt, UpdatedAt: createdAt,
		}},
	}
	w := do(newRouter(points, &fakePremium{}), http.MethodGet, "/v1/billing/points?limit=5", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	got := decode[pointsResponse](t, w)
	if got.Balance != 650 || len(got.Ledger) != 1 || len(got.Purchases) != 1 {
		t.Fatalf("unexpected response: %s", w.Body)
	}
	if e := got.Ledger[0]; e.Delta != 550 || e.Reason != "purchase" || !e.CreatedAt.Equal(createdAt) {
		t.Errorf("unexpected ledger entry: %+v", e)
	}
	if p := got.Purchases[0]; p.ID != purchaseID || p.PriceCents != 5_000_00 || p.Status != "paid" {
		t.Errorf("unexpected purchase: %+v", p)
	}
	if points.limit != 5 {
		t.Errorf("limit = %d, want 5", points.limit)
	}
}

func TestGetPointsEmptyListsAreArrays(t *testing.T) {
	w := do(newRouter(&fakePoints{}, &fakePremium{}), http.MethodGet, "/v1/billing/points", "")

	if body := w.Body.String(); !strings.Contains(body, `"ledger":[]`) || !strings.Contains(body, `"purchases":[]`) {
		t.Errorf("empty lists must be [] for the clients: %s", body)
	}
}

func TestGetPointsInvalidLimit(t *testing.T) {
	for _, limit := range []string{"abc", "0", "-1"} {
		w := do(newRouter(&fakePoints{}, &fakePremium{}), http.MethodGet, "/v1/billing/points?limit="+limit, "")
		assertError(t, w, http.StatusBadRequest, CodeInvalidRequest)
	}
}

func TestGetPointsErrors(t *testing.T) {
	for _, step := range []string{"Balance", "ListLedger", "ListPurchases"} {
		t.Run(step, func(t *testing.T) {
			w := do(newRouter(&fakePoints{err: errBoom, failOn: step}, &fakePremium{}), http.MethodGet, "/v1/billing/points", "")
			assertError(t, w, http.StatusInternalServerError, CodeInternalError)
		})
	}
}

func TestGetPurchase(t *testing.T) {
	points := &fakePoints{purchase: model.PointPurchase{
		ID: purchaseID, UserID: userID, Points: 550, Status: model.PurchaseStatusPending, CreatedAt: createdAt,
	}}
	w := do(newRouter(points, &fakePremium{}), http.MethodGet, "/v1/billing/purchases/"+purchaseID.String(), "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := decode[purchaseResponse](t, w); got.ID != purchaseID || got.Status != "pending" || got.Points != 550 {
		t.Errorf("unexpected purchase: %+v", got)
	}
}

func TestGetPurchaseNotFound(t *testing.T) {
	r := newRouter(&fakePoints{}, &fakePremium{})
	for _, id := range []string{"not-a-uuid", purchaseID.String()} {
		assertError(t, do(r, http.MethodGet, "/v1/billing/purchases/"+id, ""), http.StatusNotFound, CodePurchaseNotFound)
	}
}

func TestGetPurchaseError(t *testing.T) {
	w := do(newRouter(&fakePoints{err: errBoom}, &fakePremium{}), http.MethodGet, "/v1/billing/purchases/"+purchaseID.String(), "")
	assertError(t, w, http.StatusInternalServerError, CodeInternalError)
}

func TestGetPremium(t *testing.T) {
	until := createdAt.AddDate(0, 1, 0)
	tests := map[service.PremiumState]bool{
		service.PremiumStateNone:                   false,
		service.PremiumStateProcessingFirstPayment: false,
		service.PremiumStateActive:                 true,
		service.PremiumStateActiveCanceled:         true,
		service.PremiumStateExpired:                false,
	}
	for state, active := range tests {
		t.Run(string(state), func(t *testing.T) {
			premium := &fakePremium{view: service.PremiumView{State: state, PremiumUntil: &until, Renews: true, MPStatus: "authorized"}}
			w := do(newRouter(&fakePoints{}, premium), http.MethodGet, "/v1/billing/premium", "")

			got := decode[premiumResponse](t, w)
			if got.State != state || got.Active != active || got.PremiumUntil == nil || !got.PremiumUntil.Equal(until) ||
				!got.Renews || got.MPStatus != "authorized" {
				t.Errorf("unexpected response: %s", w.Body)
			}
			if premium.refresh {
				t.Error("refresh should be off by default")
			}
		})
	}
}

func TestGetPremiumRefresh(t *testing.T) {
	for _, q := range []string{"1", "true"} {
		premium := &fakePremium{}
		do(newRouter(&fakePoints{}, premium), http.MethodGet, "/v1/billing/premium?refresh="+q, "")
		if !premium.refresh {
			t.Errorf("refresh=%s should ask MP", q)
		}
	}
}

func TestGetPremiumError(t *testing.T) {
	w := do(newRouter(&fakePoints{}, &fakePremium{err: errBoom}), http.MethodGet, "/v1/billing/premium", "")
	assertError(t, w, http.StatusInternalServerError, CodeInternalError)
}

func TestStartPremiumCheckout(t *testing.T) {
	for name, body := range map[string]string{"with email": `{"payer_email":"ana@test.com"}`, "without body": ""} {
		t.Run(name, func(t *testing.T) {
			premium := &fakePremium{initPoint: "https://mp.test/sub"}
			w := do(newRouter(&fakePoints{}, premium), http.MethodPost, "/v1/billing/premium/checkout", body)

			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, body %s", w.Code, w.Body)
			}
			if got := decode[checkoutResponse](t, w); got.CheckoutURL != "https://mp.test/sub" || got.PurchaseID != nil {
				t.Errorf("unexpected response: %s", w.Body)
			}
			if body != "" && premium.payerEmail != "ana@test.com" {
				t.Errorf("payer email = %q", premium.payerEmail)
			}
		})
	}
}

func TestStartPremiumCheckoutErrors(t *testing.T) {
	tests := map[string]struct {
		body   string
		err    error
		status int
		code   string
	}{
		"invalid email":       {`{"payer_email":"nope"}`, nil, http.StatusBadRequest, CodeInvalidRequest},
		"already subscribed":  {"", service.ErrAlreadySubscribed, http.StatusConflict, CodeAlreadySubscribed},
		"payer email missing": {"", service.ErrPayerEmailRequired, http.StatusBadRequest, CodePayerEmailRequired},
		"internal":            {"", errBoom, http.StatusInternalServerError, CodeInternalError},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w := do(newRouter(&fakePoints{}, &fakePremium{err: tc.err}), http.MethodPost, "/v1/billing/premium/checkout", tc.body)
			assertError(t, w, tc.status, tc.code)
		})
	}
}

func TestCancelPremium(t *testing.T) {
	premium := &fakePremium{view: service.PremiumView{State: service.PremiumStateActiveCanceled, MPStatus: "cancelled"}}
	w := do(newRouter(&fakePoints{}, premium), http.MethodPost, "/v1/billing/premium/cancel", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := decode[premiumResponse](t, w); got.State != service.PremiumStateActiveCanceled || !got.Active || got.Renews {
		t.Errorf("unexpected response: %s", w.Body)
	}
}

func TestCancelPremiumWithoutSubscription(t *testing.T) {
	w := do(newRouter(&fakePoints{}, &fakePremium{err: service.ErrNoLiveSubscription}), http.MethodPost, "/v1/billing/premium/cancel", "")
	assertError(t, w, http.StatusConflict, CodeNoLiveSubscription)
}

const (
	pointsCheckoutPath  = "/v1/billing/points/checkout"
	premiumCheckoutPath = "/v1/billing/premium/checkout"
	pointsCheckoutBody  = `{"package_id":"points_100"}`
)

func TestCheckoutPassesTheIdempotencyKey(t *testing.T) {
	cases := map[string]struct {
		path, body, fingerprint string
		scope                   model.CheckoutScope
		wantURL                 string
	}{
		"points":  {pointsCheckoutPath, pointsCheckoutBody, "points_100", model.CheckoutScopePoints, "https://mp.test/c"},
		"premium": {premiumCheckoutPath, `{"payer_email":"ana@test.com"}`, premiumFingerprint, model.CheckoutScopePremium, "https://mp.test/sub"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{}
			w := doWithKey(routerWithRunner(runner), tc.path, tc.body, "key-1")

			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, body %s", w.Code, w.Body)
			}
			if got := decode[checkoutResponse](t, w).CheckoutURL; got != tc.wantURL {
				t.Errorf("url = %q, want %q", got, tc.wantURL)
			}
			want := model.CheckoutKey{UserID: userID, Scope: tc.scope, Key: "key-1"}
			if runner.key != want || runner.fingerprint != tc.fingerprint {
				t.Errorf("runner got %+v %q, want %+v %q", runner.key, runner.fingerprint, want, tc.fingerprint)
			}
		})
	}
}

func TestCheckoutIdempotencyErrors(t *testing.T) {
	for _, path := range []string{pointsCheckoutPath, premiumCheckoutPath} {
		t.Run(path, func(t *testing.T) {
			r := routerWithRunner(&fakeRunner{err: service.ErrCheckoutInProgress})
			assertError(t, doWithKey(r, path, pointsCheckoutBody, "k"), http.StatusConflict, CodeCheckoutInProgress)

			r = routerWithRunner(&fakeRunner{err: service.ErrCheckoutKeyReused})
			assertError(t, doWithKey(r, path, pointsCheckoutBody, "k"), http.StatusUnprocessableEntity, CodeIdempotencyKeyReused)

			r = routerWithRunner(&fakeRunner{})
			tooLong := strings.Repeat("k", maxIdempotencyKeyLength+1)
			assertError(t, doWithKey(r, path, pointsCheckoutBody, tooLong), http.StatusBadRequest, CodeInvalidRequest)
		})
	}
}

func TestNewBillingHandlerDefaultsLogger(t *testing.T) {
	if h := NewBillingHandler(&fakePoints{}, &fakePremium{}, &fakeRunner{}, nil); h.log == nil {
		t.Error("logger should default to slog.Default")
	}
}
