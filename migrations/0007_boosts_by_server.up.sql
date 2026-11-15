-- A user's boosts on one server, newest first: the server's boost list pages
-- through this, and the list of boosted servers groups by it.
CREATE INDEX idx_server_boosts_user_server ON server_boosts (user_id, server_id, created_at DESC);
