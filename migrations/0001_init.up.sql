CREATE TABLE point_wallets (
    user_id    UUID PRIMARY KEY,
    balance    BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE point_purchases (
    id                 UUID PRIMARY KEY,
    user_id            UUID NOT NULL,
    package_id         TEXT NOT NULL,
    points             INTEGER NOT NULL CHECK (points > 0),
    amount_cents       BIGINT NOT NULL CHECK (amount_cents > 0),
    mp_order_id        TEXT UNIQUE,
    status             TEXT NOT NULL DEFAULT 'pending'
                       CHECK (status IN ('pending', 'paid', 'failed', 'expired', 'refunded', 'charged_back')),
    status_detail      TEXT,
    -- Points that could not be taken back on a refund because the user had already spent them.
    points_unrecovered INTEGER NOT NULL DEFAULT 0 CHECK (points_unrecovered >= 0),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_point_purchases_user ON point_purchases (user_id, created_at DESC);

-- Invariant: SUM(delta) per user = point_wallets.balance.
CREATE TABLE point_ledger (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    UUID NOT NULL REFERENCES point_wallets (user_id),
    delta      BIGINT NOT NULL,
    reason     TEXT NOT NULL CHECK (reason IN ('purchase', 'refund', 'chargeback')),
    ref_id     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (reason, ref_id)
);

CREATE INDEX idx_point_ledger_user ON point_ledger (user_id, created_at DESC);

-- One row per Mercado Pago preapproval, so old subscriptions keep their history.
CREATE TABLE subscriptions (
    mp_preapproval_id TEXT PRIMARY KEY,
    user_id           UUID NOT NULL,
    mp_status         TEXT NOT NULL,
    amount_cents      BIGINT NOT NULL CHECK (amount_cents > 0),
    currency_id       TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_subscriptions_user ON subscriptions (user_id, created_at DESC);

-- A user can never have two subscriptions that MP keeps charging.
CREATE UNIQUE INDEX uq_subscriptions_live_per_user ON subscriptions (user_id)
    WHERE mp_status IN ('authorized', 'paused');

-- One row per user: what the rest of Discordia sees as "is Premium".
CREATE TABLE premium_status (
    user_id        UUID PRIMARY KEY,
    premium_until  TIMESTAMPTZ,
    renews         BOOLEAN NOT NULL DEFAULT false,
    version        BIGINT NOT NULL DEFAULT 0,
    -- Whether the last billing.premium_updated sent had active = true; the expiry job looks for these.
    active_emitted BOOLEAN NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_premium_status_expiry ON premium_status (premium_until) WHERE active_emitted;

CREATE TABLE subscription_charges (
    mp_authorized_payment_id TEXT PRIMARY KEY,
    mp_preapproval_id        TEXT NOT NULL REFERENCES subscriptions (mp_preapproval_id),
    mp_payment_id            TEXT UNIQUE,
    status                   TEXT NOT NULL,
    payment_status           TEXT,
    amount_cents             BIGINT NOT NULL,
    debit_date               TIMESTAMPTZ,
    retry_attempt            INTEGER NOT NULL DEFAULT 0,
    -- Set when this charge extended premium_until, so it is never applied twice.
    applied_at               TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_subscription_charges_preapproval ON subscription_charges (mp_preapproval_id);

CREATE TABLE outbox_events (
    id           UUID PRIMARY KEY,
    routing_key  TEXT NOT NULL,
    payload      JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    attempts     INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT
);

CREATE INDEX idx_outbox_events_pending ON outbox_events (created_at) WHERE published_at IS NULL;
