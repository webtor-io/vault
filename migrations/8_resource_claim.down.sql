DROP INDEX IF EXISTS idx_resource_claim_expires;
ALTER TABLE resource DROP COLUMN IF EXISTS claimed_by;
ALTER TABLE resource DROP COLUMN IF EXISTS claim_expires_at;
