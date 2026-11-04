package mercadopago

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/mercadopago/sdk-go/pkg/config"

	"github.com/discordia-grupo01/billing/internal/service"
)

func TestGetPointsOrder(t *testing.T) {
	client, got := newServer(t, http.StatusOK, `{
		"id":"ORD123","status":"processed","status_detail":"accredited",
		"external_reference":"purchase-42","total_amount":"1500.5"}`)

	order, err := client.GetPointsOrder(context.Background(), "ORD123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.method != http.MethodGet || got.path != "/v1/orders/ORD123" {
		t.Errorf("unexpected request %s %s", got.method, got.path)
	}
	if order.ID != "ORD123" || order.Status != "processed" || order.StatusDetail != "accredited" ||
		order.ExternalReference != "purchase-42" || order.AmountCents != 150050 {
		t.Errorf("unexpected order: %+v", order)
	}
}

// A rejected card leaves the order open; the failed attempt is in its
// payments, and the latest one is what counts.
func TestGetPointsOrderReadsLastPayment(t *testing.T) {
	client, _ := newServer(t, http.StatusOK, `{
		"id":"ORD123","status":"action_required","external_reference":"purchase-42","total_amount":"1000.00",
		"transactions":{"payments":[
			{"id":"PAY1","status":"failed","status_detail":"insufficient_amount"},
			{"id":"PAY2","status":"failed","status_detail":"rejected_by_issuer"}]}}`)

	order, err := client.GetPointsOrder(context.Background(), "ORD123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if order.LastPaymentStatus != "failed" || order.LastPaymentStatusDetail != "rejected_by_issuer" {
		t.Errorf("last payment = %s/%s, want failed/rejected_by_issuer", order.LastPaymentStatus, order.LastPaymentStatusDetail)
	}
}

func TestGetPointsOrderInvalidAmount(t *testing.T) {
	client, _ := newServer(t, http.StatusOK, `{"id":"ORD123","total_amount":"12.345"}`)

	if _, err := client.GetPointsOrder(context.Background(), "ORD123"); err == nil {
		t.Fatal("expected error for an amount with 3 decimals")
	}
}

func TestGetSubscription(t *testing.T) {
	client, got := newServer(t, http.StatusOK,
		`{"id":"PRE1","status":"authorized","external_reference":"user-7"}`)

	sub, err := client.GetSubscription(context.Background(), "PRE1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.method != http.MethodGet || got.path != "/preapproval/PRE1" {
		t.Errorf("unexpected request %s %s", got.method, got.path)
	}
	if sub.ID != "PRE1" || sub.Status != "authorized" || sub.ExternalReference != "user-7" {
		t.Errorf("unexpected subscription: %+v", sub)
	}
}

func TestGetSubscriptionCharge(t *testing.T) {
	client, got := newServer(t, http.StatusOK, `{
		"id":7001,"preapproval_id":"PRE1","status":"processed","transaction_amount":999.99,
		"debit_date":"2026-11-01T10:00:00.000-04:00","retry_attempt":2,
		"payment":{"id":555,"status":"approved","status_detail":"accredited"}}`)

	charge, err := client.GetSubscriptionCharge(context.Background(), "7001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.method != http.MethodGet || got.path != "/authorized_payments/7001" {
		t.Errorf("unexpected request %s %s", got.method, got.path)
	}
	if charge.AuthorizedPaymentID != "7001" || charge.PreapprovalID != "PRE1" || charge.Status != "processed" ||
		charge.PaymentID != "555" || charge.PaymentStatus != "approved" || charge.AmountCents != 99999 ||
		charge.RetryAttempt != 2 {
		t.Errorf("unexpected charge: %+v", charge)
	}
	want := time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)
	if charge.DebitDate == nil || !charge.DebitDate.Equal(want) {
		t.Errorf("DebitDate = %v, want %v", charge.DebitDate, want)
	}
	if charge.AppliedAt != nil {
		t.Errorf("AppliedAt must stay nil, it's ours: %v", charge.AppliedAt)
	}
}

func TestGetSubscriptionChargeNotAttemptedYet(t *testing.T) {
	client, _ := newServer(t, http.StatusOK,
		`{"id":7002,"preapproval_id":"PRE1","status":"scheduled","transaction_amount":10}`)

	charge, err := client.GetSubscriptionCharge(context.Background(), "7002")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if charge.PaymentID != "" || charge.DebitDate != nil {
		t.Errorf("expected no payment id and no debit date: %+v", charge)
	}
}

func TestListSubscriptionChargesPaginates(t *testing.T) {
	const total = chargesPageSize + 3
	var queries []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/authorized_payments/search" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		q := r.URL.Query()
		queries = append(queries, q)
		offset, _ := strconv.Atoi(q.Get("offset"))
		n := min(chargesPageSize, total-offset)
		results := ""
		for i := 0; i < n; i++ {
			if i > 0 {
				results += ","
			}
			results += fmt.Sprintf(`{"id":%d,"preapproval_id":"PRE1","status":"processed"}`, offset+i+1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"paging":{"total":%d,"offset":%d,"limit":%d},"results":[%s]}`,
			total, offset, chargesPageSize, results)
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	client, err := NewClient("TEST-TOKEN", config.WithHTTPClient(redirectRequester{target}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	charges, err := client.ListSubscriptionCharges(context.Background(), "PRE1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(charges) != total {
		t.Fatalf("got %d charges, want %d", len(charges), total)
	}
	if charges[0].AuthorizedPaymentID != "1" || charges[total-1].AuthorizedPaymentID != strconv.Itoa(total) {
		t.Errorf("unexpected order of charges: first %s, last %s",
			charges[0].AuthorizedPaymentID, charges[total-1].AuthorizedPaymentID)
	}
	if len(queries) != 2 {
		t.Fatalf("made %d requests, want 2", len(queries))
	}
	if q := queries[1]; q.Get("preapproval_id") != "PRE1" || q.Get("offset") != strconv.Itoa(chargesPageSize) ||
		q.Get("limit") != strconv.Itoa(chargesPageSize) {
		t.Errorf("unexpected second page query: %v", q)
	}
}

func TestListSubscriptionChargesEmpty(t *testing.T) {
	client, _ := newServer(t, http.StatusOK, `{"paging":{"total":0},"results":[]}`)

	charges, err := client.ListSubscriptionCharges(context.Background(), "PRE1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(charges) != 0 {
		t.Errorf("expected no charges, got %+v", charges)
	}
}

func TestGetPayment(t *testing.T) {
	client, got := newServer(t, http.StatusOK, `{
		"id":555,"status":"refunded","status_detail":"refunded",
		"external_reference":"user-7","transaction_amount":999.99}`)

	p, err := client.GetPayment(context.Background(), "555")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.method != http.MethodGet || got.path != "/v1/payments/555" {
		t.Errorf("unexpected request %s %s", got.method, got.path)
	}
	if p.ID != "555" || p.Status != "refunded" || p.StatusDetail != "refunded" ||
		p.ExternalReference != "user-7" || p.AmountCents != 99999 {
		t.Errorf("unexpected payment: %+v", p)
	}
}

func TestGetPaymentRejectsNonNumericID(t *testing.T) {
	client, got := newServer(t, http.StatusOK, `{}`)

	if _, err := client.GetPayment(context.Background(), "abc"); err == nil {
		t.Fatal("expected error")
	}
	if got.method != "" {
		t.Error("no request should be sent")
	}
}

func TestGetChargebackPaymentID(t *testing.T) {
	client, got := newServer(t, http.StatusOK, `{"id":"CB1","payment_id":555}`)

	id, err := client.GetChargebackPaymentID(context.Background(), "CB1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.method != http.MethodGet || got.path != "/v1/chargebacks/CB1" {
		t.Errorf("unexpected request %s %s", got.method, got.path)
	}
	if id != "555" {
		t.Errorf("payment id = %q, want 555", id)
	}
}

func TestGetChargebackPaymentIDWithoutPayment(t *testing.T) {
	client, _ := newServer(t, http.StatusOK, `{"id":"CB1"}`)

	if _, err := client.GetChargebackPaymentID(context.Background(), "CB1"); err == nil {
		t.Fatal("expected error for a chargeback without payment")
	}
}

func TestReadsRequireID(t *testing.T) {
	client, got := newServer(t, http.StatusOK, `{}`)
	ctx := context.Background()

	calls := map[string]func() error{
		"GetPointsOrder":          func() error { _, err := client.GetPointsOrder(ctx, ""); return err },
		"GetSubscription":         func() error { _, err := client.GetSubscription(ctx, ""); return err },
		"GetSubscriptionCharge":   func() error { _, err := client.GetSubscriptionCharge(ctx, ""); return err },
		"ListSubscriptionCharges": func() error { _, err := client.ListSubscriptionCharges(ctx, ""); return err },
		"GetChargebackPaymentID":  func() error { _, err := client.GetChargebackPaymentID(ctx, ""); return err },
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s: expected error for empty id", name)
		}
	}
	if got.method != "" {
		t.Error("no request should be sent")
	}
}

func TestReadsMapNotFound(t *testing.T) {
	client, _ := newServer(t, http.StatusNotFound, `{"message":"not found","status":404}`)
	ctx := context.Background()

	calls := map[string]func() error{
		"GetPointsOrder":          func() error { _, err := client.GetPointsOrder(ctx, "X"); return err },
		"GetSubscription":         func() error { _, err := client.GetSubscription(ctx, "X"); return err },
		"GetSubscriptionCharge":   func() error { _, err := client.GetSubscriptionCharge(ctx, "1"); return err },
		"ListSubscriptionCharges": func() error { _, err := client.ListSubscriptionCharges(ctx, "X"); return err },
		"GetPayment":              func() error { _, err := client.GetPayment(ctx, "1"); return err },
		"GetChargebackPaymentID":  func() error { _, err := client.GetChargebackPaymentID(ctx, "X"); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, service.ErrNotFound) {
			t.Errorf("%s: expected service.ErrNotFound, got %v", name, err)
		}
	}
}

// What the Orders API answers for an id like the "12345" of the panel's
// simulated notifications.
func TestInvalidPathParamIsNotFound(t *testing.T) {
	client, _ := newServer(t, http.StatusBadRequest,
		`{"errors":[{"code":"invalid_path_param","message":"path param order id is invalid"}]}`)

	_, err := client.GetPointsOrder(context.Background(), "12345")
	if !errors.Is(err, service.ErrNotFound) {
		t.Errorf("expected service.ErrNotFound, got %v", err)
	}
}

func TestReadsOtherErrorsAreNotNotFound(t *testing.T) {
	client, _ := newServer(t, http.StatusBadRequest, `{"message":"bad","status":400}`)

	_, err := client.GetSubscription(context.Background(), "X")
	if err == nil || errors.Is(err, service.ErrNotFound) {
		t.Errorf("expected a non-NotFound error, got %v", err)
	}
}

func TestParseCents(t *testing.T) {
	ok := map[string]int64{"1500": 150000, "1500.5": 150050, "1500.50": 150050, "0.07": 7, "-0.50": -50, "-12.3": -1230}
	for in, want := range ok {
		if got, err := parseCents(in); err != nil || got != want {
			t.Errorf("parseCents(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".5", "1.234", "abc", "1.x", "1.-5"} {
		if _, err := parseCents(in); err == nil {
			t.Errorf("parseCents(%q): expected error", in)
		}
	}
}

func TestFloatToCents(t *testing.T) {
	// 0.29*100 is 28.999999999999996 in float64; truncating would lose a cent.
	if got := floatToCents(0.29); got != 29 {
		t.Errorf("floatToCents(0.29) = %d, want 29", got)
	}
	if got := floatToCents(999.99); got != 99999 {
		t.Errorf("floatToCents(999.99) = %d, want 99999", got)
	}
}
