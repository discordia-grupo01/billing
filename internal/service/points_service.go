package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/discordia-grupo01/billing/internal/model"
)

var (
	ErrUnknownPackage = errors.New("unknown points package")
	// ErrOrderMismatch means the order doesn't match the purchase it points
	// to (different order id or amount). Retrying won't fix it: the caller
	// should log it and acknowledge the notification.
	ErrOrderMismatch = errors.New("order does not match its purchase")
)

const (
	defaultListLimit = 20
	maxListLimit     = 100

	// The reconciliation job leaves fresh purchases alone so it doesn't race
	// the checkout, and gives up on the ones nobody paid after a day. If MP
	// accredits one later anyway, the webhook still credits it.
	pendingCheckAfter   = 10 * time.Minute
	pendingAbandonAfter = 24 * time.Hour
	pendingBatch        = 200
	detailAbandoned     = "abandoned"
	detailNoOrder       = "order_not_created"

	// A card chargeback can arrive months after the payment.
	reversalCheckWindow = 180 * 24 * time.Hour
	paidBatch           = 200
)

type PointsPackage struct {
	ID          string
	Points      int
	AmountCents int64
	Title       string
	Description string
}

// pointsPackages is the only place to change what is sold, as (points, price
// in ARS). Everything else, including the text Mercado Pago shows, comes from
// these values. Purchases store their own points and amount, so changes only
// affect new purchases.
var pointsPackages = []PointsPackage{
	newPointsPackage(100, 1000),
	newPointsPackage(550, 5000),
	newPointsPackage(1200, 10000),
}

// newPointsPackage takes the price in whole pesos; amounts are kept in cents
// everywhere else.
func newPointsPackage(points int, pricePesos int64) PointsPackage {
	return PointsPackage{
		ID:          fmt.Sprintf("points_%d", points),
		Points:      points,
		AmountCents: pricePesos * 100,
		Title:       fmt.Sprintf("%d puntos", points),
		Description: fmt.Sprintf("Paquete de %d puntos de Discordia", points),
	}
}

// PointsStore is the slice of the points repository this service needs.
type PointsStore interface {
	CreatePurchase(ctx context.Context, p model.PointPurchase) (model.PointPurchase, error)
	SetOrderID(ctx context.Context, purchaseID uuid.UUID, orderID string) error
	GetPurchase(ctx context.Context, id uuid.UUID) (model.PointPurchase, error)
	ListPurchases(ctx context.Context, userID uuid.UUID, limit int) ([]model.PointPurchase, error)
	ListPendingPurchases(ctx context.Context, createdBefore time.Time, limit int) ([]model.PointPurchase, error)
	ListPaidPurchasesToRecheck(ctx context.Context, createdAfter time.Time, limit int) ([]model.PointPurchase, error)
	MarkReversalChecked(ctx context.Context, purchaseID uuid.UUID) error
	MarkUnpaid(ctx context.Context, purchaseID uuid.UUID, status model.PurchaseStatus, detail string) (bool, error)
	CreditPurchase(ctx context.Context, purchaseID uuid.UUID, statusDetail string) (bool, error)
	ReversePurchase(ctx context.Context, purchaseID uuid.UUID, reason model.LedgerReason) (int64, error)
	Balance(ctx context.Context, userID uuid.UUID) (int64, error)
	ListLedger(ctx context.Context, userID uuid.UUID, limit int) ([]model.LedgerEntry, error)
}

// ReturnURLs are where Mercado Pago sends the buyer back after paying. Each
// checkout adds purchase_id to them, so the return page knows what to poll.
type ReturnURLs struct {
	Success string
	Failure string
	Pending string
}

type CheckoutPayer struct {
	Email     string
	FirstName string
	LastName  string
}

type Checkout struct {
	PurchaseID  uuid.UUID
	CheckoutURL string
}

type PointsService struct {
	store   PointsStore
	gateway PaymentGateway
	urls    ReturnURLs
	log     *slog.Logger
	newID   func() uuid.UUID
	now     func() time.Time
}

func NewPointsService(store PointsStore, gateway PaymentGateway, urls ReturnURLs, logger *slog.Logger) *PointsService {
	if logger == nil {
		logger = slog.Default()
	}
	return &PointsService{store: store, gateway: gateway, urls: urls, log: logger, newID: uuid.New, now: time.Now}
}

func (s *PointsService) Packages() []PointsPackage {
	return slices.Clone(pointsPackages)
}

// StartCheckout creates the purchase and its Mercado Pago order. The
// purchase id is also the order's external_reference and idempotency key.
// If the order can't be created the purchase stays pending without an order.
func (s *PointsService) StartCheckout(ctx context.Context, userID uuid.UUID, packageID string, payer CheckoutPayer) (Checkout, error) {
	i := slices.IndexFunc(pointsPackages, func(p PointsPackage) bool { return p.ID == packageID })
	if i < 0 {
		return Checkout{}, fmt.Errorf("%w: %q", ErrUnknownPackage, packageID)
	}
	pkg := pointsPackages[i]

	purchase, err := s.store.CreatePurchase(ctx, model.PointPurchase{
		ID:          s.newID(),
		UserID:      userID,
		PackageID:   pkg.ID,
		Points:      pkg.Points,
		AmountCents: pkg.AmountCents,
	})
	if err != nil {
		return Checkout{}, err
	}

	ref := purchase.ID.String()
	order, err := s.gateway.CreatePointsOrder(ctx, model.PointsOrderRequest{
		IdempotencyKey:    ref,
		ExternalReference: ref,
		PayerEmail:        payer.Email,
		PayerFirstName:    payer.FirstName,
		PayerLastName:     payer.LastName,
		ItemID:            pkg.ID,
		Title:             pkg.Title,
		Description:       pkg.Description,
		AmountCents:       pkg.AmountCents,
		SuccessURL:        withPurchaseID(s.urls.Success, ref),
		FailureURL:        withPurchaseID(s.urls.Failure, ref),
		PendingURL:        withPurchaseID(s.urls.Pending, ref),
	})
	if err != nil {
		return Checkout{}, err
	}
	if err := s.store.SetOrderID(ctx, purchase.ID, order.ID); err != nil {
		return Checkout{}, err
	}
	return Checkout{PurchaseID: purchase.ID, CheckoutURL: order.CheckoutURL}, nil
}

func withPurchaseID(rawURL, purchaseID string) string {
	u, err := url.Parse(rawURL)
	if err != nil || rawURL == "" {
		return rawURL
	}
	q := u.Query()
	q.Set("purchase_id", purchaseID)
	u.RawQuery = q.Encode()
	return u.String()
}

// ProcessOrderNotification re-fetches the order from Mercado Pago and moves
// the purchase accordingly. It never trusts the notification body and is safe
// to run any number of times. A nil error means the notification can be
// acknowledged; ErrOrderMismatch too, but it deserves an alert.
func (s *PointsService) ProcessOrderNotification(ctx context.Context, orderID string) error {
	order, err := s.gateway.GetPointsOrder(ctx, orderID)
	if errors.Is(err, ErrNotFound) {
		s.log.Info("ignoring notification for unknown order", "order_id", orderID)
		return nil
	}
	if err != nil {
		return err
	}

	purchaseID, err := uuid.Parse(order.ExternalReference)
	if err != nil {
		s.log.Info("ignoring order not created by billing", "order_id", orderID)
		return nil
	}
	purchase, err := s.store.GetPurchase(ctx, purchaseID)
	if errors.Is(err, model.ErrNotFound) {
		s.log.Warn("ignoring order for unknown purchase", "order_id", orderID, "purchase_id", purchaseID)
		return nil
	}
	if err != nil {
		return err
	}

	if err := s.linkOrder(ctx, &purchase, order); err != nil {
		return err
	}

	switch order.Status {
	case model.OrderStatusProcessed:
		if order.StatusDetail == model.OrderDetailPartiallyRefunded {
			// Decided on purpose: points refunds are always full, from the MP panel.
			s.log.Warn("partial refund ignored, user keeps the points", "purchase_id", purchase.ID, "order_id", order.ID)
		}
		_, err = s.store.CreditPurchase(ctx, purchase.ID, order.StatusDetail)
	case model.OrderStatusFailed:
		status := model.PurchaseStatusFailed
		if order.StatusDetail == model.OrderDetailExpired {
			status = model.PurchaseStatusExpired
		}
		_, err = s.store.MarkUnpaid(ctx, purchase.ID, status, order.StatusDetail)
	case model.OrderStatusCanceled:
		_, err = s.store.MarkUnpaid(ctx, purchase.ID, model.PurchaseStatusExpired, order.StatusDetail)
	case model.OrderStatusRefunded:
		err = s.reverse(ctx, purchase, model.LedgerReasonRefund)
	case model.OrderStatusChargedBack:
		// Any chargeback detail takes the points back, and they are never
		// re-credited, even if the dispute ends in our favor.
		err = s.reverse(ctx, purchase, model.LedgerReasonChargeback)
	default:
		// created, processing, action_required, in_review: still in progress.
		// If the card was rejected the order stays open so the buyer can try
		// another one; marking the purchase failed lets the return page say
		// so. A later approved attempt still credits it.
		if order.LastPaymentStatus == model.OrderPaymentStatusFailed {
			_, err = s.store.MarkUnpaid(ctx, purchase.ID, model.PurchaseStatusFailed, order.LastPaymentStatusDetail)
		}
	}
	return err
}

// ReconcilePending asks MP about purchases that are still pending, in case
// their notification was lost, and expires the ones nobody paid. Errors on
// one purchase are logged and don't stop the rest; it returns how many
// purchases it looked at.
func (s *PointsService) ReconcilePending(ctx context.Context) (int, error) {
	now := s.now()
	purchases, err := s.store.ListPendingPurchases(ctx, now.Add(-pendingCheckAfter), pendingBatch)
	if err != nil {
		return 0, err
	}
	for _, p := range purchases {
		log := s.log.With("purchase_id", p.ID, "order_id", p.MPOrderID)
		if p.MPOrderID == "" {
			// The order was never created, so there is nothing to pay. If it
			// was created but saving its id failed, its webhook links it.
			if _, err := s.store.MarkUnpaid(ctx, p.ID, model.PurchaseStatusExpired, detailNoOrder); err != nil {
				log.Error("could not expire purchase without order", "error", err)
			}
			continue
		}
		if err := s.ProcessOrderNotification(ctx, p.MPOrderID); err != nil {
			log.Error("could not reconcile purchase", "error", err)
			continue
		}
		if p.CreatedAt.Before(now.Add(-pendingAbandonAfter)) {
			// MarkUnpaid only moves pending purchases, so one that MP just
			// accredited stays paid.
			if _, err := s.store.MarkUnpaid(ctx, p.ID, model.PurchaseStatusExpired, detailAbandoned); err != nil {
				log.Error("could not expire abandoned purchase", "error", err)
			}
		}
	}
	return len(purchases), nil
}

// ReconcilePaid asks MP about paid purchases, so the points are taken back
// even if the notification of a refund or chargeback was lost. A purchase that
// fails stays first in line for the next run.
func (s *PointsService) ReconcilePaid(ctx context.Context) (int, error) {
	purchases, err := s.store.ListPaidPurchasesToRecheck(ctx, s.now().Add(-reversalCheckWindow), paidBatch)
	if err != nil {
		return 0, err
	}
	for _, p := range purchases {
		log := s.log.With("purchase_id", p.ID, "order_id", p.MPOrderID)
		if err := s.ProcessOrderNotification(ctx, p.MPOrderID); err != nil {
			log.Error("could not recheck paid purchase", "error", err)
			continue
		}
		if err := s.store.MarkReversalChecked(ctx, p.ID); err != nil {
			log.Error("could not mark paid purchase as checked", "error", err)
		}
	}
	return len(purchases), nil
}

// linkOrder checks that the order belongs to the purchase. If saving the
// order id failed during checkout, it is saved now.
func (s *PointsService) linkOrder(ctx context.Context, purchase *model.PointPurchase, order model.PointsOrder) error {
	switch purchase.MPOrderID {
	case order.ID:
	case "":
		if err := s.store.SetOrderID(ctx, purchase.ID, order.ID); err != nil {
			return err
		}
		purchase.MPOrderID = order.ID
	default:
		return fmt.Errorf("%w: purchase %s has order %s, got %s", ErrOrderMismatch, purchase.ID, purchase.MPOrderID, order.ID)
	}
	if order.AmountCents != purchase.AmountCents {
		return fmt.Errorf("%w: purchase %s is %d cents, order %s is %d", ErrOrderMismatch,
			purchase.ID, purchase.AmountCents, order.ID, order.AmountCents)
	}
	return nil
}

// reverse takes the points back. If we missed the payment notification the
// purchase was never credited, so it is credited first: the ledger then shows
// both movements instead of a reversal with no matching purchase.
func (s *PointsService) reverse(ctx context.Context, purchase model.PointPurchase, reason model.LedgerReason) error {
	switch purchase.Status {
	case model.PurchaseStatusPending, model.PurchaseStatusFailed, model.PurchaseStatusExpired:
		if _, err := s.store.CreditPurchase(ctx, purchase.ID, ""); err != nil {
			return err
		}
	}
	_, err := s.store.ReversePurchase(ctx, purchase.ID, reason)
	return err
}

// GetPurchase returns model.ErrNotFound for other users' purchases, so ids
// can't be probed.
func (s *PointsService) GetPurchase(ctx context.Context, userID, purchaseID uuid.UUID) (model.PointPurchase, error) {
	p, err := s.store.GetPurchase(ctx, purchaseID)
	if err != nil {
		return model.PointPurchase{}, err
	}
	if p.UserID != userID {
		return model.PointPurchase{}, model.ErrNotFound
	}
	return p, nil
}

func (s *PointsService) Balance(ctx context.Context, userID uuid.UUID) (int64, error) {
	return s.store.Balance(ctx, userID)
}

func (s *PointsService) ListPurchases(ctx context.Context, userID uuid.UUID, limit int) ([]model.PointPurchase, error) {
	return s.store.ListPurchases(ctx, userID, clampLimit(limit))
}

func (s *PointsService) ListLedger(ctx context.Context, userID uuid.UUID, limit int) ([]model.LedgerEntry, error) {
	return s.store.ListLedger(ctx, userID, clampLimit(limit))
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	return min(limit, maxListLimit)
}
