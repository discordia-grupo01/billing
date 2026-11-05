-- One row per Idempotency-Key sent to a checkout. checkout_url stays NULL while
-- the checkout is in progress.
CREATE TABLE checkout_requests (
    user_id         UUID NOT NULL,
    scope           TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    fingerprint     TEXT NOT NULL,
    purchase_id     UUID,
    checkout_url    TEXT,
    locked_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, scope, idempotency_key)
);

CREATE INDEX idx_checkout_requests_created ON checkout_requests (created_at);
