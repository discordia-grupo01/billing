-- A boost spends points on a server. billing owns the points and servers owns
-- the members and the level, so a boost starts pending until servers answers.
-- The saga's state is this row; there is no separate saga table.
CREATE TABLE server_boosts (
    id                UUID PRIMARY KEY,
    user_id           UUID NOT NULL,
    server_id         UUID NOT NULL,
    points            INTEGER NOT NULL CHECK (points > 0),
    status            TEXT NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'active', 'failed', 'removed')),
    status_detail     TEXT,
    -- In the same row and transaction as the debit, so a double click can never charge twice.
    idempotency_key   TEXT,
    last_requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_server_boosts_idempotency ON server_boosts (user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_server_boosts_user ON server_boosts (user_id, created_at DESC);
CREATE INDEX idx_server_boosts_server ON server_boosts (server_id) WHERE status = 'active';
CREATE INDEX idx_server_boosts_pending ON server_boosts (created_at) WHERE status = 'pending';

ALTER TABLE point_ledger DROP CONSTRAINT point_ledger_reason_check;
ALTER TABLE point_ledger ADD CONSTRAINT point_ledger_reason_check
    CHECK (reason IN ('purchase', 'refund', 'chargeback', 'boost', 'boost_refund'));
