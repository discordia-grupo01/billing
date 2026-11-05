ALTER TABLE point_purchases DROP COLUMN IF EXISTS reversal_checked_at;
ALTER TABLE subscription_charges DROP COLUMN IF EXISTS revoked_at;
