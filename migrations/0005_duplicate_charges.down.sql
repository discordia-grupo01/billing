ALTER TABLE subscription_charges DROP COLUMN IF EXISTS refund_requested_at;
ALTER TABLE subscription_charges DROP COLUMN IF EXISTS duplicate_at;
