-- Restore path column to file table (rollback)
ALTER TABLE file
  ADD COLUMN IF NOT EXISTS path TEXT;
