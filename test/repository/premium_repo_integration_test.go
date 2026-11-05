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

	"github.com/discordia-grupo01/billing/internal/model"
	"github.com/discordia-grupo01/billing/internal/repository"
	events "github.com/discordia-grupo01/events-contract/billing"
)

var nov1 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func newPremiumRepo(t *testing.T) (*repository.PremiumRepository, *pgxpool.Pool) {
	t.Helper()
	pool := newTestPool(t)
	return repository.NewPremiumRepository(pool), pool
}

func newSubscription(t *testing.T, repo *repository.PremiumRepository, userID uuid.UUID, status string) model.SubscriptionRecord {
	t.Helper()
	s, err := repo.CreateSubscription(context.Background(), model.SubscriptionRecord{
		MPPreapprovalID: "pre-" + uuid.NewString()[:8], UserID: userID, MPStatus: status,
		AmountCents: 499900, CurrencyID: "ARS",
	})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	return s
}

func approvedCharge(preapprovalID, id string, debit time.Time) model.Charge {
	return model.Charge{
		AuthorizedPaymentID: id, PreapprovalID: preapprovalID, PaymentID: "pay-" + id,
		Status: "processed", PaymentStatus: model.PaymentStatusApproved,
		AmountCents: 499900, DebitDate: &debit,
	}
}

func premiumEvents(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) []events.PremiumUpdated {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT payload FROM outbox_events
		WHERE routing_key = $1 AND payload->>'user_id' = $2
		ORDER BY (payload->>'version')::bigint`, events.RoutingKeyPremiumUpdated, userID.String())
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var out []events.PremiumUpdated
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		var evt events.PremiumUpdated
		if err := json.Unmarshal(raw, &evt); err != nil {
			t.Fatalf("decode outbox: %v", err)
		}
		out = append(out, evt)
	}
	return out
}

func assertPremiumUntil(t *testing.T, p model.Premium, want time.Time) {
	t.Helper()
	if p.PremiumUntil == nil || !p.PremiumUntil.Equal(want) {
		t.Errorf("premium_until = %v, want %v", p.PremiumUntil, want)
	}
}

func TestCreateAndGetSubscription(t *testing.T) {
	repo, _ := newPremiumRepo(t)
	ctx := context.Background()
	userID := uuid.New()

	old := newSubscription(t, repo, userID, "cancelled")
	current := newSubscription(t, repo, userID, model.SubscriptionStatusPending)

	got, err := repo.GetSubscription(ctx, current.MPPreapprovalID)
	if err != nil || got.UserID != userID || got.MPStatus != "pending" || got.CurrencyID != "ARS" {
		t.Errorf("GetSubscription = %+v, %v", got, err)
	}
	latest, err := repo.LatestSubscription(ctx, userID)
	if err != nil || latest.MPPreapprovalID != current.MPPreapprovalID {
		t.Errorf("LatestSubscription = %s, %v; want %s (not %s)", latest.MPPreapprovalID, err, current.MPPreapprovalID, old.MPPreapprovalID)
	}
	if _, err := repo.GetSubscription(ctx, "missing"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, err := repo.LatestSubscription(ctx, uuid.New()); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSecondLiveSubscriptionIsRejected(t *testing.T) {
	repo, _ := newPremiumRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	first := newSubscription(t, repo, userID, model.SubscriptionStatusPending)
	second := newSubscription(t, repo, userID, model.SubscriptionStatusPending)

	if _, err := repo.SyncSubscriptionStatus(ctx, first.MPPreapprovalID, model.SubscriptionStatusAuthorized); err != nil {
		t.Fatalf("authorize first: %v", err)
	}
	if _, err := repo.SyncSubscriptionStatus(ctx, second.MPPreapprovalID, model.SubscriptionStatusAuthorized); !errors.Is(err, repository.ErrLiveSubscriptionExists) {
		t.Errorf("authorize second: err = %v, want ErrLiveSubscriptionExists", err)
	}
	if _, err := repo.CreateSubscription(ctx, model.SubscriptionRecord{
		MPPreapprovalID: "pre-third", UserID: userID, MPStatus: model.SubscriptionStatusAuthorized,
		AmountCents: 499900, CurrencyID: "ARS",
	}); !errors.Is(err, repository.ErrLiveSubscriptionExists) {
		t.Errorf("create live: err = %v, want ErrLiveSubscriptionExists", err)
	}
}

func TestSyncSubscriptionStatusTogglesRenews(t *testing.T) {
	repo, pool := newPremiumRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	s := newSubscription(t, repo, userID, model.SubscriptionStatusPending)

	changed, err := repo.SyncSubscriptionStatus(ctx, s.MPPreapprovalID, model.SubscriptionStatusAuthorized)
	if err != nil || !changed {
		t.Fatalf("authorize = %v, %v", changed, err)
	}
	again, err := repo.SyncSubscriptionStatus(ctx, s.MPPreapprovalID, model.SubscriptionStatusAuthorized)
	if err != nil || again {
		t.Errorf("repeated authorize = %v, %v; want no change", again, err)
	}
	if p, _ := repo.GetPremium(ctx, userID); !p.Renews || p.Version != 1 {
		t.Errorf("after authorize: renews=%v version=%d", p.Renews, p.Version)
	}

	if changed, err := repo.SyncSubscriptionStatus(ctx, s.MPPreapprovalID, model.SubscriptionStatusCancelled); err != nil || !changed {
		t.Fatalf("cancel = %v, %v", changed, err)
	}
	p, _ := repo.GetPremium(ctx, userID)
	if p.Renews || p.Version != 2 {
		t.Errorf("after cancel: renews=%v version=%d", p.Renews, p.Version)
	}
	evts := premiumEvents(t, pool, userID)
	if len(evts) != 2 || !evts[0].Renews || evts[1].Renews || evts[1].Active {
		t.Errorf("events = %+v", evts)
	}

	if _, err := repo.SyncSubscriptionStatus(ctx, "missing", "authorized"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSyncOldSubscriptionKeepsCurrentRenewal(t *testing.T) {
	repo, _ := newPremiumRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	old := newSubscription(t, repo, userID, model.SubscriptionStatusPending)
	current := newSubscription(t, repo, userID, model.SubscriptionStatusPending)
	if _, err := repo.SyncSubscriptionStatus(ctx, current.MPPreapprovalID, model.SubscriptionStatusAuthorized); err != nil {
		t.Fatal(err)
	}

	changed, err := repo.SyncSubscriptionStatus(ctx, old.MPPreapprovalID, model.SubscriptionStatusCancelled)
	if err != nil || changed {
		t.Errorf("cancel old = %v, %v; want no premium change", changed, err)
	}
	if p, _ := repo.GetPremium(ctx, userID); !p.Renews {
		t.Error("cancelling the old subscription turned off the current renewal")
	}
}

func TestApplyChargeExtendsOnce(t *testing.T) {
	repo, pool := newPremiumRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	s := newSubscription(t, repo, userID, model.SubscriptionStatusAuthorized)

	if ok, _ := repo.HasAppliedCharge(ctx, s.MPPreapprovalID); ok {
		t.Error("HasAppliedCharge before any charge = true")
	}
	c := approvedCharge(s.MPPreapprovalID, "ap1", nov1)
	extended, err := repo.ApplyCharge(ctx, c)
	if err != nil || !extended {
		t.Fatalf("ApplyCharge = %v, %v", extended, err)
	}
	again, err := repo.ApplyCharge(ctx, c)
	if err != nil || again {
		t.Errorf("second ApplyCharge = %v, %v; want no-op", again, err)
	}

	p, _ := repo.GetPremium(ctx, userID)
	assertPremiumUntil(t, p, nov1.AddDate(0, 1, 0))
	if p.Version != 1 || !p.ActiveEmitted {
		t.Errorf("version=%d active_emitted=%v", p.Version, p.ActiveEmitted)
	}
	if ok, _ := repo.HasAppliedCharge(ctx, s.MPPreapprovalID); !ok {
		t.Error("HasAppliedCharge after approved charge = false")
	}
	evts := premiumEvents(t, pool, userID)
	if len(evts) != 1 || !evts[0].Active || evts[0].EventID == "" || evts[0].Version != 1 {
		t.Errorf("events = %+v", evts)
	}
}

func TestApplyChargeConcurrentDeliveriesExtendOnce(t *testing.T) {
	repo, _ := newPremiumRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	s := newSubscription(t, repo, userID, model.SubscriptionStatusAuthorized)
	c := approvedCharge(s.MPPreapprovalID, "ap-concurrent", nov1)

	var wg sync.WaitGroup
	var mu sync.Mutex
	extensions := 0
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := repo.ApplyCharge(ctx, c)
			if err != nil {
				t.Errorf("ApplyCharge: %v", err)
			}
			if ok {
				mu.Lock()
				extensions++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if extensions != 1 {
		t.Errorf("extended %d times, want 1", extensions)
	}
	p, _ := repo.GetPremium(ctx, userID)
	assertPremiumUntil(t, p, nov1.AddDate(0, 1, 0))
}

func TestRenewalAndLateRetry(t *testing.T) {
	repo, _ := newPremiumRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	s := newSubscription(t, repo, userID, model.SubscriptionStatusAuthorized)
	dec1 := nov1.AddDate(0, 1, 0)

	if _, err := repo.ApplyCharge(ctx, approvedCharge(s.MPPreapprovalID, "nov", nov1)); err != nil {
		t.Fatal(err)
	}

	// The December charge is rejected: premium_until does not move.
	rejected := approvedCharge(s.MPPreapprovalID, "dec", dec1)
	rejected.Status, rejected.PaymentStatus, rejected.RetryAttempt = "recycling", "rejected", 1
	if ok, err := repo.ApplyCharge(ctx, rejected); err != nil || ok {
		t.Fatalf("rejected charge = %v, %v; want not extended", ok, err)
	}
	p, _ := repo.GetPremium(ctx, userID)
	assertPremiumUntil(t, p, dec1)

	// MP retries on Dec 5 and it goes through: Premium restarts from that day.
	dec5 := dec1.AddDate(0, 0, 4)
	retry := approvedCharge(s.MPPreapprovalID, "dec", dec5)
	retry.RetryAttempt = 2
	if ok, err := repo.ApplyCharge(ctx, retry); err != nil || !ok {
		t.Fatalf("retry = %v, %v; want extended", ok, err)
	}
	p, _ = repo.GetPremium(ctx, userID)
	assertPremiumUntil(t, p, dec5.AddDate(0, 1, 0))

	// A renewal charged before premium_until stacks on top of it.
	early := approvedCharge(s.MPPreapprovalID, "jan", dec5.AddDate(0, 0, 25))
	if _, err := repo.ApplyCharge(ctx, early); err != nil {
		t.Fatal(err)
	}
	p, _ = repo.GetPremium(ctx, userID)
	assertPremiumUntil(t, p, dec5.AddDate(0, 2, 0))
}

func TestApplyChargeUnknownSubscription(t *testing.T) {
	repo, _ := newPremiumRepo(t)
	if _, err := repo.ApplyCharge(context.Background(), approvedCharge("missing", "ap", nov1)); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRevokeCharge(t *testing.T) {
	repo, pool := newPremiumRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	s := newSubscription(t, repo, userID, model.SubscriptionStatusPending)
	if _, err := repo.SyncSubscriptionStatus(ctx, s.MPPreapprovalID, model.SubscriptionStatusAuthorized); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Truncate(time.Second)
	c := approvedCharge(s.MPPreapprovalID, "ap1", future)
	if _, err := repo.ApplyCharge(ctx, c); err != nil {
		t.Fatal(err)
	}

	rev, revoked, err := repo.RevokeCharge(ctx, c.PaymentID, "charged_back")
	if err != nil || !revoked {
		t.Fatalf("RevokeCharge = %v, %v", revoked, err)
	}
	if rev.UserID != userID || rev.PreapprovalID != s.MPPreapprovalID {
		t.Errorf("revocation = %+v", rev)
	}
	p, _ := repo.GetPremium(ctx, userID)
	if p.Active(time.Now()) || p.Renews || p.ActiveEmitted {
		t.Errorf("after revoke: %+v", p)
	}
	evts := premiumEvents(t, pool, userID)
	if last := evts[len(evts)-1]; last.Active || last.Renews {
		t.Errorf("last event = %+v, want inactive", last)
	}

	if _, again, err := repo.RevokeCharge(ctx, c.PaymentID, "charged_back"); err != nil || again {
		t.Errorf("second revoke = %v, %v; want no-op", again, err)
	}
	if got := len(premiumEvents(t, pool, userID)); got != len(evts) {
		t.Errorf("second revoke wrote an event: %d -> %d", len(evts), got)
	}
	if _, _, err := repo.RevokeCharge(ctx, "missing", "refunded"); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// The reconciliation can store the refunded status before anything revokes.
func TestRevokeChargeAfterApplyChargeStoredTheReversal(t *testing.T) {
	repo, pool := newPremiumRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	s := newSubscription(t, repo, userID, model.SubscriptionStatusAuthorized)
	c := approvedCharge(s.MPPreapprovalID, "ap1", time.Now().UTC().Truncate(time.Second))
	if _, err := repo.ApplyCharge(ctx, c); err != nil {
		t.Fatal(err)
	}

	c.PaymentStatus = "refunded"
	if _, err := repo.ApplyCharge(ctx, c); err != nil {
		t.Fatal(err)
	}
	_, revoked, err := repo.RevokeCharge(ctx, c.PaymentID, "refunded")
	if err != nil || !revoked {
		t.Fatalf("RevokeCharge = %v, %v; want revoked", revoked, err)
	}
	if p, _ := repo.GetPremium(ctx, userID); p.Active(time.Now()) || p.Renews {
		t.Errorf("after revoke: %+v", p)
	}

	evts := len(premiumEvents(t, pool, userID))
	if _, again, err := repo.RevokeCharge(ctx, c.PaymentID, "charged_back"); err != nil || again {
		t.Errorf("second revoke = %v, %v; want no-op", again, err)
	}
	if got := len(premiumEvents(t, pool, userID)); got != evts {
		t.Errorf("second revoke wrote an event: %d -> %d", evts, got)
	}
}

func TestRevokeChargeThatNeverExtended(t *testing.T) {
	repo, _ := newPremiumRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	s := newSubscription(t, repo, userID, model.SubscriptionStatusAuthorized)
	c := approvedCharge(s.MPPreapprovalID, "ap1", nov1)
	c.PaymentStatus = "in_process"
	if _, err := repo.ApplyCharge(ctx, c); err != nil {
		t.Fatal(err)
	}

	if _, revoked, err := repo.RevokeCharge(ctx, c.PaymentID, "refunded"); err != nil || revoked {
		t.Errorf("RevokeCharge = %v, %v; want recorded but not revoked", revoked, err)
	}
	if p, _ := repo.GetPremium(ctx, userID); p.Version != 0 {
		t.Errorf("version = %d, want 0 (no event)", p.Version)
	}
}

func TestExpireDue(t *testing.T) {
	repo, pool := newPremiumRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	s := newSubscription(t, repo, userID, model.SubscriptionStatusAuthorized)
	if _, err := repo.ApplyCharge(ctx, approvedCharge(s.MPPreapprovalID, "ap1", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}

	if n, err := repo.ExpireDue(ctx, 10); err != nil || n != 0 {
		t.Fatalf("ExpireDue before expiry = %d, %v", n, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE premium_status SET premium_until = now() - interval '1 minute' WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	n, err := repo.ExpireDue(ctx, 10)
	if err != nil || n != 1 {
		t.Fatalf("ExpireDue = %d, %v; want 1", n, err)
	}
	if n, _ := repo.ExpireDue(ctx, 10); n != 0 {
		t.Errorf("second ExpireDue = %d, want 0", n)
	}
	p, _ := repo.GetPremium(ctx, userID)
	if p.ActiveEmitted || p.Version != 2 {
		t.Errorf("after expiry: active_emitted=%v version=%d", p.ActiveEmitted, p.Version)
	}
	evts := premiumEvents(t, pool, userID)
	if len(evts) != 2 || evts[1].Active || evts[1].Version != 2 {
		t.Errorf("events = %+v", evts)
	}
}

func TestListSubscriptionsToReconcile(t *testing.T) {
	repo, _ := newPremiumRepo(t)
	ctx := context.Background()
	newSubscription(t, repo, uuid.New(), model.SubscriptionStatusPending)
	newSubscription(t, repo, uuid.New(), model.SubscriptionStatusAuthorized)
	newSubscription(t, repo, uuid.New(), model.SubscriptionStatusCancelled)
	newSubscription(t, repo, uuid.New(), model.SubscriptionStatusPaused)

	subs, err := repo.ListSubscriptionsToReconcile(ctx, 10)
	if err != nil || len(subs) != 3 {
		t.Fatalf("got %d, %v; want pending + authorized + paused", len(subs), err)
	}
	for _, s := range subs {
		if s.MPStatus == model.SubscriptionStatusCancelled {
			t.Errorf("a cancelled subscription that never paid should be left out: %+v", s)
		}
	}
}

func TestListSubscriptionsToReconcileCancelledWithPaidPeriod(t *testing.T) {
	repo, pool := newPremiumRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	s := newSubscription(t, repo, userID, model.SubscriptionStatusAuthorized)
	c := approvedCharge(s.MPPreapprovalID, "ap1", time.Now().UTC())
	if _, err := repo.ApplyCharge(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SyncSubscriptionStatus(ctx, s.MPPreapprovalID, model.SubscriptionStatusCancelled); err != nil {
		t.Fatal(err)
	}

	listed := func() bool {
		t.Helper()
		subs, err := repo.ListSubscriptionsToReconcile(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range subs {
			if got.MPPreapprovalID == s.MPPreapprovalID {
				return true
			}
		}
		return false
	}
	if !listed() {
		t.Error("should be listed while the paid period runs")
	}

	if _, err := pool.Exec(ctx,
		`UPDATE premium_status SET premium_until = now() - interval '1 minute' WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if listed() {
		t.Error("should not be listed after the paid period ended")
	}
}

func TestGetPremiumForNewUser(t *testing.T) {
	repo, _ := newPremiumRepo(t)
	userID := uuid.New()
	p, err := repo.GetPremium(context.Background(), userID)
	if err != nil || p.UserID != userID || p.PremiumUntil != nil || p.Active(time.Now()) {
		t.Errorf("GetPremium = %+v, %v", p, err)
	}
}

func TestEndPremiumPeriod(t *testing.T) {
	repo, pool := newPremiumRepo(t)
	ctx := context.Background()
	userID := uuid.New()
	s := newSubscription(t, repo, userID, model.SubscriptionStatusAuthorized)
	if _, err := repo.ApplyCharge(ctx, approvedCharge(s.MPPreapprovalID, "ap1", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}

	ended, err := repo.EndPremiumPeriod(ctx, userID)
	if err != nil || !ended {
		t.Fatalf("EndPremiumPeriod = %v, %v; want true", ended, err)
	}
	if p, _ := repo.GetPremium(ctx, userID); p.Active(time.Now()) {
		t.Errorf("Premium should be over: until %v", p.PremiumUntil)
	}
	// The expiry job picks it up and publishes active=false.
	if n, err := repo.ExpireDue(ctx, 10); err != nil || n != 1 {
		t.Fatalf("ExpireDue = %d, %v; want 1", n, err)
	}
	if evts := premiumEvents(t, pool, userID); evts[len(evts)-1].Active {
		t.Errorf("last event should say active=false: %+v", evts[len(evts)-1])
	}

	if ended, err := repo.EndPremiumPeriod(ctx, userID); err != nil || ended {
		t.Errorf("second EndPremiumPeriod = %v, %v; want false", ended, err)
	}
	if ended, err := repo.EndPremiumPeriod(ctx, uuid.New()); err != nil || ended {
		t.Errorf("EndPremiumPeriod for a user without Premium = %v, %v; want false", ended, err)
	}
}

func TestRepublishAll(t *testing.T) {
	repo, pool := newPremiumRepo(t)
	ctx := context.Background()

	paying := uuid.New()
	s := newSubscription(t, repo, paying, model.SubscriptionStatusAuthorized)
	if _, err := repo.ApplyCharge(ctx, approvedCharge(s.MPPreapprovalID, "ap1", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	expired := uuid.New()
	s = newSubscription(t, repo, expired, model.SubscriptionStatusAuthorized)
	if _, err := repo.ApplyCharge(ctx, approvedCharge(s.MPPreapprovalID, "ap2", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE premium_status SET premium_until = now() - interval '1 minute' WHERE user_id = $1`, expired); err != nil {
		t.Fatal(err)
	}
	before, _ := repo.GetPremium(ctx, paying)

	n, err := repo.RepublishAll(ctx)
	if err != nil || n != 2 {
		t.Fatalf("RepublishAll = %d, %v; want 2", n, err)
	}

	evts := premiumEvents(t, pool, paying)
	last := evts[len(evts)-1]
	if !last.Active || last.UserID != paying.String() || last.Version != before.Version+1 {
		t.Errorf("paying user event = %+v, previous version %d", last, before.Version)
	}
	if last.PremiumUntil == nil || !last.PremiumUntil.Equal(*before.PremiumUntil) {
		t.Errorf("premium_until = %v, want %v", last.PremiumUntil, before.PremiumUntil)
	}

	evts = premiumEvents(t, pool, expired)
	if evts[len(evts)-1].Active {
		t.Error("a user whose period passed must be republished as inactive")
	}
	if p, _ := repo.GetPremium(ctx, expired); p.ActiveEmitted {
		t.Error("active_emitted should follow what the event said")
	}
	if n, _ := repo.ExpireDue(ctx, 10); n != 0 {
		t.Errorf("ExpireDue after republishing = %d, want 0: it would emit twice", n)
	}

	if n, err := repo.RepublishAll(ctx); err != nil || n != 2 {
		t.Fatalf("second RepublishAll = %d, %v; want 2", n, err)
	}
	if p, _ := repo.GetPremium(ctx, paying); p.Version != before.Version+2 {
		t.Errorf("version after running twice = %d, want %d", p.Version, before.Version+2)
	}
}

func TestRepublishAllWithNobody(t *testing.T) {
	repo, _ := newPremiumRepo(t)
	if n, err := repo.RepublishAll(context.Background()); err != nil || n != 0 {
		t.Fatalf("RepublishAll = %d, %v; want 0", n, err)
	}
}

func TestRepublishAllDBError(t *testing.T) {
	repo, pool := newPremiumRepo(t)
	pool.Close()
	if _, err := repo.RepublishAll(context.Background()); err == nil {
		t.Fatal("RepublishAll should fail with a closed pool")
	}
}
