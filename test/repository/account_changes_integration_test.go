package repository_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/discordia-grupo01/billing/internal/model"
	"github.com/discordia-grupo01/billing/internal/repository"
	"github.com/discordia-grupo01/events-contract/account"
	"github.com/discordia-grupo01/events-contract/validate"
)

func accountChanges(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT payload FROM outbox_events WHERE routing_key = $1 ORDER BY created_at, id`, account.RoutingKeyChanged)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if err := validate.Validate(account.RoutingKeyChanged, payload); err != nil {
			t.Errorf("the event does not meet the contract: %v", err)
		}
		var e account.Changed
		if err := json.Unmarshal(payload, &e); err != nil {
			t.Fatal(err)
		}
		if e.UserID == userID.String() {
			out = append(out, e.Resource)
		}
	}
	return out
}

func count(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}

func TestAccountChanged_APurchaseTellsPurchasesWhenCreatedAndPurchasesAndPointsWhenCredited(t *testing.T) {
	repo, pool := newPointsRepo(t)
	user := uuid.New()

	p := newOrderedPurchase(t, repo, user, 500)
	if got := accountChanges(t, pool, user); len(got) != 1 || got[0] != "purchases" {
		t.Fatalf("after creating = %v, want [purchases]", got)
	}

	if ok, err := repo.CreditPurchase(context.Background(), p.ID, "accredited"); err != nil || !ok {
		t.Fatalf("CreditPurchase = %v, %v", ok, err)
	}
	got := accountChanges(t, pool, user)
	if count(got, "points") != 1 || count(got, "purchases") != 2 {
		t.Errorf("after crediting = %v, want one points and two purchases", got)
	}
}

func TestAccountChanged_ARepeatedCreditTellsNothingNew(t *testing.T) {
	repo, pool := newPointsRepo(t)
	user := uuid.New()
	p := newPaidPurchase(t, repo, user, 300)
	before := len(accountChanges(t, pool, user))

	if ok, err := repo.CreditPurchase(context.Background(), p.ID, "accredited"); err != nil || ok {
		t.Fatalf("second CreditPurchase = %v, %v, want false", ok, err)
	}

	if after := len(accountChanges(t, pool, user)); after != before {
		t.Errorf("events grew from %d to %d on a repeated credit", before, after)
	}
}

func TestAccountChanged_AReversalTellsPointsAndPurchases(t *testing.T) {
	repo, pool := newPointsRepo(t)
	user := uuid.New()
	p := newPaidPurchase(t, repo, user, 400)
	before := accountChanges(t, pool, user)

	if _, err := repo.ReversePurchase(context.Background(), p.ID, model.LedgerReasonRefund); err != nil {
		t.Fatalf("ReversePurchase: %v", err)
	}

	after := accountChanges(t, pool, user)
	if count(after, "points") != count(before, "points")+1 || count(after, "purchases") != count(before, "purchases")+1 {
		t.Errorf("before %v, after %v: want one more points and one more purchases", before, after)
	}
}

func TestAccountChanged_AnUnpaidPurchaseTellsPurchasesOnlyWhenItMoved(t *testing.T) {
	repo, pool := newPointsRepo(t)
	user := uuid.New()
	p := newOrderedPurchase(t, repo, user, 100)
	before := len(accountChanges(t, pool, user))

	if ok, err := repo.MarkUnpaid(context.Background(), p.ID, model.PurchaseStatusFailed, "rejected"); err != nil || !ok {
		t.Fatalf("MarkUnpaid = %v, %v", ok, err)
	}
	if got := len(accountChanges(t, pool, user)); got != before+1 {
		t.Fatalf("events = %d, want %d", got, before+1)
	}

	if ok, _ := repo.MarkUnpaid(context.Background(), p.ID, model.PurchaseStatusFailed, "again"); ok {
		t.Fatal("a resolved purchase must not move again")
	}
	if got := len(accountChanges(t, pool, user)); got != before+1 {
		t.Errorf("events = %d after a no-op, want %d", got, before+1)
	}
}

func TestAccountChanged_ABoostTellsPointsAndBoostsAtEveryStep(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	ctx := context.Background()
	user, server := uuid.New(), uuid.New()
	fund(t, pool, user, 500)

	b, _, err := repo.Create(ctx, newBoost(user, server), "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := accountChanges(t, pool, user)
	if count(got, "points") != 1 || count(got, "boosts") != 1 {
		t.Fatalf("after reserving = %v, want one points and one boosts", got)
	}

	if ok, err := repo.MarkApplied(ctx, b.ID); err != nil || !ok {
		t.Fatalf("MarkApplied = %v, %v", ok, err)
	}
	if count(accountChanges(t, pool, user), "boosts") != 2 {
		t.Errorf("applying did not tell the boosts: %v", accountChanges(t, pool, user))
	}

	if _, err := repo.Remove(ctx, user, b.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if count(accountChanges(t, pool, user), "boosts") != 3 {
		t.Errorf("removing did not tell the boosts: %v", accountChanges(t, pool, user))
	}
}

func TestAccountChanged_ARejectedBoostGivesThePointsBackAndTellsIt(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	ctx := context.Background()
	user := uuid.New()
	fund(t, pool, user, 500)
	b, _, err := repo.Create(ctx, newBoost(user, uuid.New()), "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before := accountChanges(t, pool, user)

	if ok, err := repo.Reject(ctx, b.ID, "server_not_found"); err != nil || !ok {
		t.Fatalf("Reject = %v, %v", ok, err)
	}

	after := accountChanges(t, pool, user)
	if count(after, "points") != count(before, "points")+1 || count(after, "boosts") != count(before, "boosts")+1 {
		t.Errorf("before %v, after %v: want the refund and the status change told", before, after)
	}
}

func TestAccountChanged_BoostsOfADeletedServerTellEachOwnerOnce(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	ctx := context.Background()
	server := uuid.New()
	a, b := uuid.New(), uuid.New()
	fund(t, pool, a, 1000)
	fund(t, pool, b, 1000)
	for _, user := range []uuid.UUID{a, a, b} {
		boost, _, err := repo.Create(ctx, newBoost(user, server), "")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if ok, err := repo.MarkApplied(ctx, boost.ID); err != nil || !ok {
			t.Fatalf("MarkApplied = %v, %v", ok, err)
		}
	}
	beforeA, beforeB := count(accountChanges(t, pool, a), "boosts"), count(accountChanges(t, pool, b), "boosts")

	n, err := repo.RemoveForServer(ctx, server)
	if err != nil || n != 3 {
		t.Fatalf("RemoveForServer = %d, %v, want 3", n, err)
	}

	if got := count(accountChanges(t, pool, a), "boosts") - beforeA; got != 1 {
		t.Errorf("owner with two boosts was told %d times, want once", got)
	}
	if got := count(accountChanges(t, pool, b), "boosts") - beforeB; got != 1 {
		t.Errorf("owner with one boost was told %d times, want once", got)
	}
}

func TestAccountChanged_NothingIsPublishedWhenAReservationFails(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	user := uuid.New()
	fund(t, pool, user, 10)

	if _, _, err := repo.Create(context.Background(), newBoost(user, uuid.New()), ""); err == nil {
		t.Fatal("expected insufficient points")
	}

	if got := accountChanges(t, pool, user); len(got) != 0 {
		t.Errorf("a rolled back reservation published %v", got)
	}
}
