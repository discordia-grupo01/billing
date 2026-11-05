package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/discordia-grupo01/events-contract/boosts"
	"github.com/discordia-grupo01/events-contract/envelope"
	"github.com/discordia-grupo01/events-contract/servers"

	"github.com/discordia-grupo01/billing/internal/model"
)

const boostSecret = "boost-secret"

type fakeBoostStore struct {
	created        model.Boost
	wasCreated     bool
	createErr      error
	gotBoost       model.Boost
	gotKey         string
	got            model.Boost
	getErr         error
	list           []model.Boost
	limit          int
	applied        []uuid.UUID
	rejected       map[uuid.UUID]string
	removedServers []uuid.UUID
	err            error
	before         time.Time
	batch          int
}

func (f *fakeBoostStore) Create(_ context.Context, b model.Boost, key string) (model.Boost, bool, error) {
	f.gotBoost, f.gotKey = b, key
	return f.created, f.wasCreated, f.createErr
}
func (f *fakeBoostStore) Get(context.Context, uuid.UUID) (model.Boost, error) { return f.got, f.getErr }
func (f *fakeBoostStore) ListForUser(_ context.Context, _ uuid.UUID, limit int) ([]model.Boost, error) {
	f.limit = limit
	return f.list, nil
}
func (f *fakeBoostStore) MarkApplied(_ context.Context, id uuid.UUID) (bool, error) {
	f.applied = append(f.applied, id)
	return true, f.err
}
func (f *fakeBoostStore) Reject(_ context.Context, id uuid.UUID, detail string) (bool, error) {
	if f.rejected == nil {
		f.rejected = map[uuid.UUID]string{}
	}
	f.rejected[id] = detail
	return true, f.err
}
func (f *fakeBoostStore) Remove(_ context.Context, _, _ uuid.UUID) (model.Boost, error) {
	return f.got, f.err
}
func (f *fakeBoostStore) RemoveForServer(_ context.Context, id uuid.UUID) (int, error) {
	f.removedServers = append(f.removedServers, id)
	return 1, f.err
}
func (f *fakeBoostStore) RepublishPending(_ context.Context, before time.Time, limit int) (int, error) {
	f.before, f.batch = before, limit
	return 2, f.err
}
func (f *fakeBoostStore) CancelExpired(_ context.Context, before time.Time, limit int) (int, error) {
	f.before, f.batch = before, limit
	return 3, f.err
}

func newBoostService(store BoostStore) *BoostService {
	return NewBoostService(store, boostSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func signedAnswer(t *testing.T, key, secret string, boostID, reason string) []byte {
	t.Helper()
	evt := boosts.SagaEvent{
		Meta:     envelope.Meta{EventID: uuid.NewString(), OccurredAt: time.Now()},
		BoostID:  boostID,
		ServerID: uuid.NewString(),
		UserID:   uuid.NewString(),
		Reason:   reason,
	}
	evt.Sign(key, secret)
	body, err := json.Marshal(evt)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestBoostRequestCostsOneHundredPoints(t *testing.T) {
	store := &fakeBoostStore{wasCreated: true, created: model.Boost{Status: model.BoostStatusPending}}
	user, server := uuid.New(), uuid.New()

	got, err := newBoostService(store).Request(context.Background(), user, server, "key")
	if err != nil {
		t.Fatalf("Request() error = %v", err)
	}
	if got.Status != model.BoostStatusPending {
		t.Errorf("got %+v", got)
	}
	b := store.gotBoost
	if b.ID == uuid.Nil || b.UserID != user || b.ServerID != server || b.Points != 100 || store.gotKey != "key" {
		t.Errorf("boost = %+v, key = %q", b, store.gotKey)
	}
}

func TestBoostRequestSameKeyReturnsTheFirstBoost(t *testing.T) {
	server := uuid.New()
	first := model.Boost{ID: uuid.New(), ServerID: server}
	got, err := newBoostService(&fakeBoostStore{created: first}).Request(context.Background(), uuid.New(), server, "key")
	if err != nil || got.ID != first.ID {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestBoostRequestSameKeyForAnotherServerIsAnError(t *testing.T) {
	store := &fakeBoostStore{created: model.Boost{ID: uuid.New(), ServerID: uuid.New()}}
	_, err := newBoostService(store).Request(context.Background(), uuid.New(), uuid.New(), "key")
	if !errors.Is(err, ErrCheckoutKeyReused) {
		t.Errorf("error = %v", err)
	}
}

func TestBoostRequestPassesInsufficientPointsThrough(t *testing.T) {
	store := &fakeBoostStore{createErr: model.InsufficientPointsError{Balance: 30, Cost: 100}}
	_, err := newBoostService(store).Request(context.Background(), uuid.New(), uuid.New(), "")
	var insufficient model.InsufficientPointsError
	if !errors.As(err, &insufficient) || insufficient.Missing() != 70 {
		t.Errorf("error = %v", err)
	}
}

func TestBoostGetHidesOtherUsersBoosts(t *testing.T) {
	owner := uuid.New()
	store := &fakeBoostStore{got: model.Boost{ID: uuid.New(), UserID: owner}}
	svc := newBoostService(store)

	if got, err := svc.Get(context.Background(), owner, store.got.ID); err != nil || got.ID != store.got.ID {
		t.Errorf("owner: %+v, %v", got, err)
	}
	if _, err := svc.Get(context.Background(), uuid.New(), store.got.ID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("stranger: %v", err)
	}
	store.getErr = errBoom
	if _, err := svc.Get(context.Background(), owner, store.got.ID); !errors.Is(err, errBoom) {
		t.Errorf("store failure: %v", err)
	}
}

func TestBoostListClampsTheLimit(t *testing.T) {
	store := &fakeBoostStore{}
	svc := newBoostService(store)
	for give, want := range map[int]int{0: defaultBoostLimit, 10: 10, 1000: maxBoostLimit} {
		if _, err := svc.List(context.Background(), uuid.New(), give); err != nil {
			t.Fatal(err)
		}
		if store.limit != want {
			t.Errorf("limit(%d) = %d, want %d", give, store.limit, want)
		}
	}
}

func TestBoostJobsUseTheirWindows(t *testing.T) {
	store := &fakeBoostStore{}
	svc := newBoostService(store)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }

	if n, err := svc.RepublishPending(context.Background()); err != nil || n != 2 {
		t.Errorf("RepublishPending() = %d, %v", n, err)
	}
	if !store.before.Equal(now.Add(-5*time.Minute)) || store.batch != boostBatchSize {
		t.Errorf("before = %v, batch = %d", store.before, store.batch)
	}
	if n, err := svc.CancelExpired(context.Background()); err != nil || n != 3 {
		t.Errorf("CancelExpired() = %d, %v", n, err)
	}
	if !store.before.Equal(now.Add(-24 * time.Hour)) {
		t.Errorf("before = %v", store.before)
	}
}

func TestHandleEventApplied(t *testing.T) {
	store := &fakeBoostStore{}
	id := uuid.New()
	body := signedAnswer(t, boosts.RoutingKeyApplied, boostSecret, id.String(), "")

	if err := newBoostService(store).HandleEvent(context.Background(), boosts.RoutingKeyApplied, body); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(store.applied, []uuid.UUID{id}) || store.rejected != nil {
		t.Errorf("applied = %v, rejected = %v", store.applied, store.rejected)
	}
}

func TestHandleEventRejectedRefunds(t *testing.T) {
	store := &fakeBoostStore{}
	id := uuid.New()
	body := signedAnswer(t, boosts.RoutingKeyRejected, boostSecret, id.String(), boosts.ReasonServerSuspended)

	if err := newBoostService(store).HandleEvent(context.Background(), boosts.RoutingKeyRejected, body); err != nil {
		t.Fatal(err)
	}
	if store.rejected[id] != boosts.ReasonServerSuspended || store.applied != nil {
		t.Errorf("rejected = %v, applied = %v", store.rejected, store.applied)
	}
}

func TestHandleEventServerDeleted(t *testing.T) {
	store := &fakeBoostStore{}
	server := uuid.New()
	body, err := json.Marshal(servers.ServerDeleted{ServerID: server.String()})
	if err != nil {
		t.Fatal(err)
	}
	if err := newBoostService(store).HandleEvent(context.Background(), servers.RoutingKeyServerDeleted, body); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(store.removedServers, []uuid.UUID{server}) {
		t.Errorf("removed = %v", store.removedServers)
	}
}

// A forged rejection would give points away, and a bad event requeued would loop.
func TestHandleEventDropsWhatItCannotTrust(t *testing.T) {
	id := uuid.NewString()
	cases := map[string]struct {
		key  string
		body []byte
	}{
		"another secret":      {boosts.RoutingKeyRejected, signedAnswer(t, boosts.RoutingKeyRejected, "forged", id, boosts.ReasonNotAMember)},
		"applied as rejected": {boosts.RoutingKeyRejected, signedAnswer(t, boosts.RoutingKeyApplied, boostSecret, id, "")},
		"no signature":        {boosts.RoutingKeyRejected, []byte(`{"boost_id":"` + id + `"}`)},
		"malformed":           {boosts.RoutingKeyApplied, []byte(`not json`)},
		"bad boost id":        {boosts.RoutingKeyApplied, signedAnswer(t, boosts.RoutingKeyApplied, boostSecret, "nope", "")},
		"unknown key":         {"servers.something_else", []byte(`{}`)},
		"bad deleted body":    {servers.RoutingKeyServerDeleted, []byte(`not json`)},
		"bad server id":       {servers.RoutingKeyServerDeleted, []byte(`{"server_id":"nope"}`)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := &fakeBoostStore{}
			if err := newBoostService(store).HandleEvent(context.Background(), tc.key, tc.body); err != nil {
				t.Errorf("error = %v, want it dropped", err)
			}
			if store.applied != nil || store.rejected != nil || store.removedServers != nil {
				t.Errorf("touched the store: %+v", store)
			}
		})
	}
}

func TestHandleEventRetriesOnStoreFailure(t *testing.T) {
	id := uuid.NewString()
	deleted, err := json.Marshal(servers.ServerDeleted{ServerID: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		boosts.RoutingKeyApplied:        signedAnswer(t, boosts.RoutingKeyApplied, boostSecret, id, ""),
		boosts.RoutingKeyRejected:       signedAnswer(t, boosts.RoutingKeyRejected, boostSecret, id, boosts.ReasonNotAMember),
		servers.RoutingKeyServerDeleted: deleted,
	}
	for key, body := range cases {
		store := &fakeBoostStore{err: errBoom}
		if err := newBoostService(store).HandleEvent(context.Background(), key, body); !errors.Is(err, errBoom) {
			t.Errorf("%s: error = %v", key, err)
		}
	}
}

func TestServersEventsRoutingKeys(t *testing.T) {
	want := []string{boosts.RoutingKeyApplied, boosts.RoutingKeyRejected, servers.RoutingKeyServerDeleted}
	if got := ServersEventsRoutingKeys(); !slices.Equal(got, want) {
		t.Errorf("got %v", got)
	}
}
