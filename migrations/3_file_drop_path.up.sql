-- Drop path column from file table
ALTER TABLE file
  DROP COLUMN IF EXISTS path;
