package repository_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/discordia-grupo01/events-contract/boosts"

	"github.com/discordia-grupo01/billing/internal/model"
	"github.com/discordia-grupo01/billing/internal/repository"
)

func newBoost(userID, serverID uuid.UUID) model.Boost {
	return model.Boost{ID: uuid.New(), UserID: userID, ServerID: serverID, Points: 100}
}

func fund(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, points int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO point_wallets (user_id, balance) VALUES ($1, $2)`, userID, points); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO point_ledger (user_id, delta, reason, ref_id) VALUES ($1, $2, 'purchase', $3)`,
		userID, points, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
}

// checkBalance also checks the invariant SUM(delta) = balance.
func checkBalance(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, want int64) {
	t.Helper()
	var balance, sum int64
	err := pool.QueryRow(context.Background(), `
		SELECT w.balance, COALESCE((SELECT SUM(delta) FROM point_ledger l WHERE l.user_id = w.user_id), 0)
		FROM point_wallets w WHERE w.user_id = $1`, userID).Scan(&balance, &sum)
	if err != nil {
		t.Fatal(err)
	}
	if balance != want || sum != want {
		t.Errorf("balance = %d, ledger sum = %d, want %d", balance, sum, want)
	}
}

func boostEvents(t *testing.T, pool *pgxpool.Pool, key string) []boosts.SagaEvent {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT payload FROM outbox_events WHERE routing_key = $1 ORDER BY created_at`, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []boosts.SagaEvent
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var e boosts.SagaEvent
		if err := json.Unmarshal(payload, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func status(t *testing.T, repo *repository.BoostRepository, id uuid.UUID) (model.BoostStatus, string) {
	t.Helper()
	b, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Status, b.StatusDetail
}

func TestBoostCreateDebitsAndAsksServers(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	ctx := context.Background()
	user, server := uuid.New(), uuid.New()
	fund(t, pool, user, 250)

	b, created, err := repo.Create(ctx, newBoost(user, server), "")
	if err != nil || !created {
		t.Fatalf("Create() = %v, %v", created, err)
	}
	if b.Status != model.BoostStatusPending || b.Points != 100 {
		t.Errorf("boost = %+v", b)
	}
	checkBalance(t, pool, user, 150)

	events := boostEvents(t, pool, boosts.RoutingKeyRequested)
	if len(events) != 1 || events[0].BoostID != b.ID.String() || events[0].ServerID != server.String() ||
		events[0].UserID != user.String() || events[0].EventID == "" {
		t.Errorf("events = %+v", events)
	}
}

func TestBoostCreateWithoutEnoughPointsLeavesNothing(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	user := uuid.New()
	fund(t, pool, user, 30)

	_, _, err := repo.Create(context.Background(), newBoost(user, uuid.New()), "key-1")

	var insufficient model.InsufficientPointsError
	if !errors.As(err, &insufficient) || insufficient.Missing() != 70 {
		t.Fatalf("error = %v", err)
	}
	checkBalance(t, pool, user, 30)
	list, err := repo.ListForUser(context.Background(), user, 10)
	if err != nil || len(list) != 0 {
		t.Errorf("list = %+v, err %v", list, err)
	}
	if events := boostEvents(t, pool, boosts.RoutingKeyRequested); len(events) != 0 {
		t.Errorf("events = %+v", events)
	}
}

func TestBoostCreateUserWithoutWallet(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)

	_, _, err := repo.Create(context.Background(), newBoost(uuid.New(), uuid.New()), "")

	var insufficient model.InsufficientPointsError
	if !errors.As(err, &insufficient) || insufficient.Missing() != 100 {
		t.Errorf("error = %v", err)
	}
}

func TestBoostCreateSameKeyChargesOnce(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	ctx := context.Background()
	user, server := uuid.New(), uuid.New()
	fund(t, pool, user, 500)

	first, created, err := repo.Create(ctx, newBoost(user, server), "click")
	if err != nil || !created {
		t.Fatalf("first = %v, %v", created, err)
	}
	second, created, err := repo.Create(ctx, newBoost(user, server), "click")
	if err != nil || created {
		t.Fatalf("second = %v, %v", created, err)
	}
	if second.ID != first.ID {
		t.Errorf("got another boost: %s != %s", second.ID, first.ID)
	}
	checkBalance(t, pool, user, 400)

	other := uuid.New()
	fund(t, pool, other, 100)
	if _, created, err := repo.Create(ctx, newBoost(other, server), "click"); err != nil || !created {
		t.Errorf("the same key from another user is a different request: %v, %v", created, err)
	}
}

func TestBoostCreateConcurrentSameKeyChargesOnce(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	user, server := uuid.New(), uuid.New()
	fund(t, pool, user, 1000)

	const attempts = 8
	var wg sync.WaitGroup
	createdCount := make([]bool, attempts)
	errs := make([]error, attempts)
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, createdCount[i], errs[i] = repo.Create(context.Background(), newBoost(user, server), "double-click")
		}()
	}
	wg.Wait()

	total := 0
	for i := range attempts {
		if errs[i] != nil {
			t.Errorf("attempt %d: %v", i, errs[i])
		}
		if createdCount[i] {
			total++
		}
	}
	if total != 1 {
		t.Errorf("created %d boosts", total)
	}
	checkBalance(t, pool, user, 900)
}

func TestBoostCreateConcurrentNeverOverspends(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	user := uuid.New()
	fund(t, pool, user, 250)

	const attempts = 8
	var wg sync.WaitGroup
	errs := make([]error, attempts)
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = repo.Create(context.Background(), newBoost(user, uuid.New()), "")
		}()
	}
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		var insufficient model.InsufficientPointsError
		switch {
		case err == nil:
			succeeded++
		case !errors.As(err, &insufficient):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if succeeded != 2 {
		t.Errorf("succeeded = %d, want 2", succeeded)
	}
	checkBalance(t, pool, user, 50)
}

func TestBoostMarkAppliedOnlyMovesPending(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	ctx := context.Background()
	user := uuid.New()
	fund(t, pool, user, 100)
	b, _, err := repo.Create(ctx, newBoost(user, uuid.New()), "")
	if err != nil {
		t.Fatal(err)
	}

	if moved, err := repo.MarkApplied(ctx, b.ID); err != nil || !moved {
		t.Fatalf("first = %v, %v", moved, err)
	}
	if moved, err := repo.MarkApplied(ctx, b.ID); err != nil || moved {
		t.Errorf("repeated = %v, %v", moved, err)
	}
	if st, _ := status(t, repo, b.ID); st != model.BoostStatusActive {
		t.Errorf("status = %s", st)
	}
	checkBalance(t, pool, user, 0)

	if refunded, err := repo.Reject(ctx, b.ID, boosts.ReasonNotAMember); err != nil || refunded {
		t.Errorf("a rejection after being applied must do nothing: %v, %v", refunded, err)
	}
	checkBalance(t, pool, user, 0)
}

func TestBoostRejectRefundsOnce(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	ctx := context.Background()
	user := uuid.New()
	fund(t, pool, user, 100)
	b, _, err := repo.Create(ctx, newBoost(user, uuid.New()), "")
	if err != nil {
		t.Fatal(err)
	}
	checkBalance(t, pool, user, 0)

	if refunded, err := repo.Reject(ctx, b.ID, boosts.ReasonNotAMember); err != nil || !refunded {
		t.Fatalf("Reject() = %v, %v", refunded, err)
	}
	checkBalance(t, pool, user, 100)
	if st, detail := status(t, repo, b.ID); st != model.BoostStatusFailed || detail != boosts.ReasonNotAMember {
		t.Errorf("status = %s, %s", st, detail)
	}

	if refunded, err := repo.Reject(ctx, b.ID, boosts.ReasonNotAMember); err != nil || refunded {
		t.Errorf("repeated = %v, %v", refunded, err)
	}
	checkBalance(t, pool, user, 100)

	if moved, err := repo.MarkApplied(ctx, b.ID); err != nil || moved {
		t.Errorf("applied after rejected = %v, %v", moved, err)
	}
}

func TestBoostRemove(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	ctx := context.Background()
	user, stranger := uuid.New(), uuid.New()
	fund(t, pool, user, 200)
	b, _, err := repo.Create(ctx, newBoost(user, uuid.New()), "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := repo.Remove(ctx, user, b.ID); !errors.Is(err, model.ErrBoostNotActive) {
		t.Errorf("removing a pending boost: %v", err)
	}
	if _, err := repo.MarkApplied(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Remove(ctx, stranger, b.ID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("someone else's boost: %v", err)
	}
	if _, err := repo.Remove(ctx, user, uuid.New()); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown boost: %v", err)
	}

	removed, err := repo.Remove(ctx, user, b.ID)
	if err != nil || removed.Status != model.BoostStatusRemoved {
		t.Fatalf("Remove() = %+v, %v", removed, err)
	}
	checkBalance(t, pool, user, 100)
	if events := boostEvents(t, pool, boosts.RoutingKeyRemoved); len(events) != 1 || events[0].BoostID != b.ID.String() {
		t.Errorf("events = %+v", events)
	}

	if _, err := repo.Remove(ctx, user, b.ID); !errors.Is(err, model.ErrBoostNotActive) {
		t.Errorf("removing twice: %v", err)
	}
	if events := boostEvents(t, pool, boosts.RoutingKeyRemoved); len(events) != 1 {
		t.Errorf("events after second remove = %d", len(events))
	}
}

func TestBoostRemoveForServerKeepsThePoints(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	ctx := context.Background()
	user, server := uuid.New(), uuid.New()
	fund(t, pool, user, 300)
	active, _, err := repo.Create(ctx, newBoost(user, server), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkApplied(ctx, active.ID); err != nil {
		t.Fatal(err)
	}
	pending, _, err := repo.Create(ctx, newBoost(user, server), "")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := repo.Create(ctx, newBoost(user, uuid.New()), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkApplied(ctx, other.ID); err != nil {
		t.Fatal(err)
	}

	n, err := repo.RemoveForServer(ctx, server)
	if err != nil || n != 1 {
		t.Fatalf("RemoveForServer() = %d, %v", n, err)
	}
	if st, detail := status(t, repo, active.ID); st != model.BoostStatusRemoved || detail != model.BoostDetailServerDeleted {
		t.Errorf("active = %s, %s", st, detail)
	}
	if st, _ := status(t, repo, pending.ID); st != model.BoostStatusPending {
		t.Errorf("pending = %s", st)
	}
	if st, _ := status(t, repo, other.ID); st != model.BoostStatusActive {
		t.Errorf("other server = %s", st)
	}
	checkBalance(t, pool, user, 0)
	if events := boostEvents(t, pool, boosts.RoutingKeyRemoved); len(events) != 0 {
		t.Errorf("events = %+v", events)
	}
}

func TestBoostRepublishPending(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	ctx := context.Background()
	user := uuid.New()
	fund(t, pool, user, 300)
	pending, _, err := repo.Create(ctx, newBoost(user, uuid.New()), "")
	if err != nil {
		t.Fatal(err)
	}
	applied, _, err := repo.Create(ctx, newBoost(user, uuid.New()), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkApplied(ctx, applied.ID); err != nil {
		t.Fatal(err)
	}

	if n, err := repo.RepublishPending(ctx, time.Now().Add(-time.Hour), 10); err != nil || n != 0 {
		t.Errorf("too recent = %d, %v", n, err)
	}
	if n, err := repo.RepublishPending(ctx, time.Now().Add(time.Hour), 10); err != nil || n != 1 {
		t.Fatalf("RepublishPending() = %d, %v", n, err)
	}
	events := boostEvents(t, pool, boosts.RoutingKeyRequested)
	if len(events) != 3 || events[2].BoostID != pending.ID.String() {
		t.Errorf("events = %+v", events)
	}
	if n, err := repo.RepublishPending(ctx, time.Now().Add(-time.Minute), 10); err != nil || n != 0 {
		t.Errorf("just republished = %d, %v", n, err)
	}
	checkBalance(t, pool, user, 100)
}

func TestBoostCancelExpiredRefundsAndTellsServers(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	ctx := context.Background()
	user := uuid.New()
	fund(t, pool, user, 200)
	expired, _, err := repo.Create(ctx, newBoost(user, uuid.New()), "")
	if err != nil {
		t.Fatal(err)
	}
	applied, _, err := repo.Create(ctx, newBoost(user, uuid.New()), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkApplied(ctx, applied.ID); err != nil {
		t.Fatal(err)
	}
	checkBalance(t, pool, user, 0)

	if n, err := repo.CancelExpired(ctx, time.Now().Add(-time.Hour), 10); err != nil || n != 0 {
		t.Errorf("too recent = %d, %v", n, err)
	}
	if n, err := repo.CancelExpired(ctx, time.Now().Add(time.Hour), 10); err != nil || n != 1 {
		t.Fatalf("CancelExpired() = %d, %v", n, err)
	}
	checkBalance(t, pool, user, 100)
	if st, detail := status(t, repo, expired.ID); st != model.BoostStatusFailed || detail != model.BoostDetailNoResponse {
		t.Errorf("expired = %s, %s", st, detail)
	}
	if events := boostEvents(t, pool, boosts.RoutingKeyCancelled); len(events) != 1 || events[0].BoostID != expired.ID.String() {
		t.Errorf("events = %+v", events)
	}

	if moved, err := repo.MarkApplied(ctx, expired.ID); err != nil || moved {
		t.Errorf("a late answer must not revive it: %v, %v", moved, err)
	}
	if n, err := repo.CancelExpired(ctx, time.Now().Add(time.Hour), 10); err != nil || n != 0 {
		t.Errorf("second run = %d, %v", n, err)
	}
	checkBalance(t, pool, user, 100)
}

func TestBoostGetAndList(t *testing.T) {
	pool := newTestPool(t)
	repo := repository.NewBoostRepository(pool)
	ctx := context.Background()
	user := uuid.New()
	fund(t, pool, user, 200)
	first, _, err := repo.Create(ctx, newBoost(user, uuid.New()), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.Create(ctx, newBoost(user, uuid.New()), ""); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Get(ctx, first.ID)
	if err != nil || got.ID != first.ID {
		t.Errorf("Get() = %+v, %v", got, err)
	}
	if _, err := repo.Get(ctx, uuid.New()); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	list, err := repo.ListForUser(ctx, user, 10)
	if err != nil || len(list) != 2 {
		t.Errorf("list = %+v, %v", list, err)
	}
	if list, err := repo.ListForUser(ctx, uuid.New(), 10); err != nil || len(list) != 0 {
		t.Errorf("other user = %+v, %v", list, err)
	}
}

func TestBoostMigrationRejectsUnknownStatus(t *testing.T) {
	pool := newTestPool(t)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO server_boosts (id, user_id, server_id, points, status) VALUES ($1, $2, $3, 100, 'nope')`,
		uuid.New(), uuid.New(), uuid.New())
	if err == nil {
		t.Error("expected a check violation")
	}
}
