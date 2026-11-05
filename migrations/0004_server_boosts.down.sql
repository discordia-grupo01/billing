-- Fails if boost movements exist: the wallets' history would stop adding up.
ALTER TABLE point_ledger DROP CONSTRAINT point_ledger_reason_check;
ALTER TABLE point_ledger ADD CONSTRAINT point_ledger_reason_check
    CHECK (reason IN ('purchase', 'refund', 'chargeback'));

DROP TABLE server_boosts;
