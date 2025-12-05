-- Down migration for initial schema (singular tables)

-- Drop triggers
DROP TRIGGER IF EXISTS trg_resource_set_updated_at ON resource;
DROP TRIGGER IF EXISTS trg_file_set_updated_at ON file;

-- Drop tables
DROP TABLE IF EXISTS resource_file;
DROP TABLE IF EXISTS file;
DROP TABLE IF EXISTS resource;

-- Drop helper function
DROP FUNCTION IF EXISTS set_updated_at();
