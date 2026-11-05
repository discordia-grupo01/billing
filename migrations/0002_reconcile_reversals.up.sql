-- Set when a refund or chargeback of this charge ended Premium, so it is revoked only once.
ALTER TABLE subscription_charges ADD COLUMN revoked_at TIMESTAMPTZ;

UPDATE subscription_charges SET revoked_at = updated_at
WHERE applied_at IS NOT NULL AND payment_status IN ('refunded', 'charged_back');

-- Last time the reconciliation job asked MP about this paid purchase.
ALTER TABLE point_purchases ADD COLUMN reversal_checked_at TIMESTAMPTZ;
