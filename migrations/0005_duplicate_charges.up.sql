-- Set when an approved charge did not extend Premium because another
-- subscription of the same user was live: it is refunded instead.
ALTER TABLE subscription_charges ADD COLUMN duplicate_at TIMESTAMPTZ;

-- Set once MP accepted the refund of a duplicate charge, so it is asked only once.
ALTER TABLE subscription_charges ADD COLUMN refund_requested_at TIMESTAMPTZ;
