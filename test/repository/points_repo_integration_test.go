package repository_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/discordia-grupo01/billing/internal/model"
	"github.com/discordia-grupo01/billing/internal/repository"
)

func newPointsRepo(t *testing.T) (*repository.PointsRepository, *pgxpool.Pool) {
	t.Helper()
	pool := newTestPool(t)
	return repository.NewPointsRepository(pool), pool
}

// newPaidPurchase leaves a purchase of `points` already credited to userID.
func newPaidPurchase(t *testing.T, repo *repository.PointsRepository, userID uuid.UUID, points int) model.PointPurchase {
	t.Helper()
	p := newOrderedPurchase(t, repo, userID, points)
	if ok, err := repo.CreditPurchase(context.Background(), p.ID, "accredited"); err != nil || !ok {
		t.Fatalf("CreditPurchase = %v, %v", ok, err)
	}
	return p
}

func newOrderedPurchase(t *testing.T, repo *repository.PointsRepository, userID uuid.UUID, points int) model.PointPurchase {
	t.Helper()
	ctx := context.Background()
	p, err := repo.CreatePurchase(ctx, model.PointPurchase{
		ID: uuid.New(), UserID: userID, PackageID: "points_test", Points: points, AmountCents: 100000,
	})
	if err != nil {
		t.Fatalf("CreatePurchase: %v", err)
	}
	orderID := "ORD" + p.ID.String()[:8]
	if err := repo.SetOrderID(ctx, p.ID, orderID); err != nil {
		t.Fatalf("SetOrderID: %v", err)
	}
	p.MPOrderID = orderID
	return p
}

// assertLedgerMatchesBalance checks the invariant SUM(delta) = balance.
func assertLedgerMatchesBalance(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, want int64) {
	t.Helper()
	var balance, sum int64
	err := pool.QueryRow(context.Background(), `
		SELECT w.balance, COALESCE((SELECT SUM(delta) FROM point_ledger WHERE user_id = w.user_id), 0)
		FROM point_wallets w WHERE w.user_id = $1`, userID).Scan(&balance, &sum)
	if err != nil {
		t.Fatalf("read wallet: %v", err)
	}
	if balance != want || sum != want {
		t.Errorf("balance = %d, ledger sum = %d, want both %d", balance, sum, want)
	}
}

func TestCreateAndGetPurchase(t *testing.T) {
	repo, _ := newPointsRepo(t)
	ctx := context.Background()
	userID := uuid.New()

	created, err := repo.CreatePurchase(ctx, model.PointPurchase{
		ID: uuid.New(), UserID: userID, PackageID: "points_550", Points: 550, AmountCents: 499900,
	})
	if err != nil {
		t.Fatalf("CreatePurchase: %v", err)
	}
	if created.Status != model.PurchaseStatusPending || created.MPOrderID != "" || created.CreatedAt.IsZero() {
		t.Errorf("unexpected new purchase: %+v", created)
	}

	if err := repo.SetOrderID(ctx, created.ID, "ORD01"); err != nil {
		t.Fatalf("SetOrderID: %v", err)
	}
	byID, err := repo.GetPurchase(ctx, created.ID)
	if err != nil || byID.MPOrderID != "ORD01" || byID.Points != 550 || byID.AmountCents != 499900 {
		t.Fatalf("GetPurchase = %+v, %v", byID, err)
	}
	byOrder, err := repo.GetPurchaseByOrderID(ctx, "ORD01")
	if err != nil || byOrder.ID != created.ID {
		t.Fatalf("GetPurchaseByOrderID = %+v, %v", byOrder, err)
	}
}

func TestPurchaseNotFound(t *testing.T) {
	repo, _ := newPointsRepo(t)
	ctx := context.Background()

	if _, err := repo.GetPurchase(ctx, uuid.New()); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("GetPurchase err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetPurchaseByOrderID(ctx, "missing"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("GetPurchaseByOrderID err = %v, want ErrNotFound", err)
	}
	if err := repo.SetOrderID(ctx, uuid.New(), "ORD"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("SetOrderID err = %v, want ErrNotFound", err)
	}
	if _, err := repo.CreditPurchase(ctx, uuid.New(), ""); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("CreditPurchase err = %v, want ErrNotFound", err)
	}
}

func TestCreditPurchaseIsIdempotent(t *testing.T) {
	repo, pool := newPointsRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	p := newOrderedPurchase(t, repo, userID, 550)

	for i, want := range []bool{true, false} {
		ok, err := repo.CreditPurchase(ctx, p.ID, "accredited")
		if err != nil || ok != want {
			t.Fatalf("call %d: CreditPurchase = %v, %v; want %v", i+1, ok, err, want)
		}
	}

	assertLedgerMatchesBalance(t, pool, userID, 550)
	got, _ := repo.GetPurchase(ctx, p.ID)
	if got.Status != model.PurchaseStatusPaid || got.StatusDetail != "accredited" {
		t.Errorf("purchase after credit: %+v", got)
	}
}

func TestCreditPurchaseConcurrentNotificationsCreditOnce(t *testing.T) {
	repo, pool := newPointsRepo(t)
	userID := uuid.New()
	p := newOrderedPurchase(t, repo, userID, 100)

	var wg sync.WaitGroup
	results := make(chan bool, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := repo.CreditPurchase(context.Background(), p.ID, "accredited")
			if err != nil {
				t.Errorf("CreditPurchase: %v", err)
			}
			results <- ok
		}()
	}
	wg.Wait()
	close(results)

	credited := 0
	for ok := range results {
		if ok {
			credited++
		}
	}
	if credited != 1 {
		t.Errorf("credited %d times, want 1", credited)
	}
	assertLedgerMatchesBalance(t, pool, userID, 100)
}

func TestCreditPurchaseRequiresOrderID(t *testing.T) {
	repo, _ := newPointsRepo(t)
	p, err := repo.CreatePurchase(context.Background(), model.PointPurchase{
		ID: uuid.New(), UserID: uuid.New(), PackageID: "x", Points: 1, AmountCents: 1,
	})
	if err != nil {
		t.Fatalf("CreatePurchase: %v", err)
	}
	if _, err := repo.CreditPurchase(context.Background(), p.ID, ""); !errors.Is(err, repository.ErrInvalidTransition) {
		t.Errorf("err = %v, want ErrInvalidTransition", err)
	}
}

func TestMarkUnpaidOnlyMovesPendingPurchases(t *testing.T) {
	repo, _ := newPointsRepo(t)
	ctx := context.Background()
	userID := uuid.New()

	pending := newOrderedPurchase(t, repo, userID, 100)
	if ok, err := repo.MarkUnpaid(ctx, pending.ID, model.PurchaseStatusFailed, "rejected_by_issuer"); err != nil || !ok {
		t.Fatalf("MarkUnpaid pending = %v, %v", ok, err)
	}
	got, _ := repo.GetPurchase(ctx, pending.ID)
	if got.Status != model.PurchaseStatusFailed || got.StatusDetail != "rejected_by_issuer" {
		t.Errorf("after MarkUnpaid: %+v", got)
	}

	paid := newPaidPurchase(t, repo, userID, 100)
	if ok, err := repo.MarkUnpaid(ctx, paid.ID, model.PurchaseStatusExpired, "expired"); err != nil || ok {
		t.Fatalf("MarkUnpaid paid = %v, %v; a late notification must not undo a payment", ok, err)
	}
	if got, _ := repo.GetPurchase(ctx, paid.ID); got.Status != model.PurchaseStatusPaid {
		t.Errorf("paid purchase changed to %s", got.Status)
	}

	if _, err := repo.MarkUnpaid(ctx, pending.ID, model.PurchaseStatusPaid, ""); err == nil {
		t.Error("MarkUnpaid should reject statuses other than failed/expired")
	}
}

func TestCreditPurchaseAfterFailedStillCredits(t *testing.T) {
	repo, pool := newPointsRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	p := newOrderedPurchase(t, repo, userID, 100)

	if _, err := repo.MarkUnpaid(ctx, p.ID, model.PurchaseStatusExpired, ""); err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.CreditPurchase(ctx, p.ID, "accredited"); err != nil || !ok {
		t.Fatalf("CreditPurchase after expired = %v, %v", ok, err)
	}
	assertLedgerMatchesBalance(t, pool, userID, 100)
}

func TestReversePurchaseWithFullBalance(t *testing.T) {
	repo, pool := newPointsRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	p := newPaidPurchase(t, repo, userID, 550)

	deducted, err := repo.ReversePurchase(ctx, p.ID, model.LedgerReasonRefund)
	if err != nil || deducted != 550 {
		t.Fatalf("ReversePurchase = %d, %v", deducted, err)
	}
	again, err := repo.ReversePurchase(ctx, p.ID, model.LedgerReasonRefund)
	if err != nil || again != 0 {
		t.Fatalf("second ReversePurchase = %d, %v; want 0, nil", again, err)
	}

	assertLedgerMatchesBalance(t, pool, userID, 0)
	got, _ := repo.GetPurchase(ctx, p.ID)
	if got.Status != model.PurchaseStatusRefunded || got.PointsUnrecovered != 0 {
		t.Errorf("after refund: %+v", got)
	}
}

func TestReversePurchaseOnlyTakesWhatIsLeft(t *testing.T) {
	repo, pool := newPointsRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	p := newPaidPurchase(t, repo, userID, 550)

	// The user spent 300 points. Boosts don't exist yet, so the spend is
	// written by hand; 'refund' is only used because the CHECK has no 'boost'.
	if _, err := pool.Exec(ctx,
		`INSERT INTO point_ledger (user_id, delta, reason, ref_id) VALUES ($1, -300, 'refund', 'simulated-spend')`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE point_wallets SET balance = balance - 300 WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	deducted, err := repo.ReversePurchase(ctx, p.ID, model.LedgerReasonChargeback)
	if err != nil || deducted != 250 {
		t.Fatalf("ReversePurchase = %d, %v; want 250", deducted, err)
	}
	assertLedgerMatchesBalance(t, pool, userID, 0)
	got, _ := repo.GetPurchase(ctx, p.ID)
	if got.Status != model.PurchaseStatusChargedBack || got.PointsUnrecovered != 300 {
		t.Errorf("after chargeback: %+v", got)
	}
}

func TestReversePurchaseRejectsUnpaid(t *testing.T) {
	repo, _ := newPointsRepo(t)
	ctx := context.Background()
	p := newOrderedPurchase(t, repo, uuid.New(), 100)

	if _, err := repo.ReversePurchase(ctx, p.ID, model.LedgerReasonRefund); !errors.Is(err, repository.ErrInvalidTransition) {
		t.Errorf("err = %v, want ErrInvalidTransition", err)
	}
	if _, err := repo.ReversePurchase(ctx, p.ID, "boost"); err == nil {
		t.Error("expected error for unknown reason")
	}
	if _, err := repo.ReversePurchase(ctx, uuid.New(), model.LedgerReasonRefund); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestBalanceAndHistory(t *testing.T) {
	repo, _ := newPointsRepo(t)
	ctx := context.Background()
	userID := uuid.New()

	if b, err := repo.Balance(ctx, userID); err != nil || b != 0 {
		t.Fatalf("Balance for new user = %d, %v", b, err)
	}

	first := newPaidPurchase(t, repo, userID, 100)
	second := newPaidPurchase(t, repo, userID, 550)
	newOrderedPurchase(t, repo, uuid.New(), 1200) // someone else's

	if b, err := repo.Balance(ctx, userID); err != nil || b != 650 {
		t.Fatalf("Balance = %d, %v; want 650", b, err)
	}

	purchases, err := repo.ListPurchases(ctx, userID, 10)
	if err != nil || len(purchases) != 2 || purchases[0].ID != second.ID || purchases[1].ID != first.ID {
		t.Fatalf("ListPurchases = %+v, %v; want newest first, only this user", purchases, err)
	}
	if limited, _ := repo.ListPurchases(ctx, userID, 1); len(limited) != 1 {
		t.Errorf("limit not applied: %d purchases", len(limited))
	}

	ledger, err := repo.ListLedger(ctx, userID, 10)
	if err != nil || len(ledger) != 2 || ledger[0].Delta != 550 || ledger[0].RefID != second.MPOrderID ||
		ledger[0].Reason != model.LedgerReasonPurchase {
		t.Fatalf("ListLedger = %+v, %v", ledger, err)
	}
}
