-- Lease-based claim for resource processing.
--
-- Replaces the fragile updated_at heuristic with an explicit lease:
--   claim_expires_at — deadline for the current lease (NULL = unclaimed)
--   claimed_by       — identity of the worker holding the lease
--
-- A worker acquires a resource via atomic CAS:
--   UPDATE resource
--      SET claim_expires_at = now() + interval '2 minutes',
--          claimed_by       = <worker-id>,
--          status           = <new processing status>
--    WHERE resource_id      = $1
--      AND status IN (...)
--      AND (claim_expires_at IS NULL OR claim_expires_at < now())
--   RETURNING *;
-- Either 1 row is updated (lease acquired) or 0 rows (someone else holds it).
--
-- The lease is refreshed by a background heartbeat inside handleStore /
-- handleDelete every 30s, and released (set back to NULL) on completion or
-- best-effort on graceful shutdown.

ALTER TABLE resource ADD COLUMN claim_expires_at TIMESTAMPTZ;
ALTER TABLE resource ADD COLUMN claimed_by TEXT;

-- Partial index for fast "unclaimed or expired" lookup during claim attempts.
-- Excludes terminal status=stored (2) so it stays small.
CREATE INDEX IF NOT EXISTS idx_resource_claim_expires
  ON resource (claim_expires_at)
  WHERE status != 2;
