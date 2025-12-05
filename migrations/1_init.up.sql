-- Vault initial schema (singular tables, typed statuses, bytea hash)

-- Helper: trigger to update updated_at column
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- resource table
CREATE TABLE IF NOT EXISTS resource (
  resource_id TEXT PRIMARY KEY,
  status      SMALLINT NOT NULL,
  total_size  BIGINT NOT NULL DEFAULT 0 CHECK (total_size >= 0),
  stored_size BIGINT NOT NULL DEFAULT 0 CHECK (stored_size >= 0 AND stored_size <= total_size),
  error       TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_resource_status ON resource(status);

-- file table
CREATE TABLE IF NOT EXISTS file (
  hash        text PRIMARY KEY,
  status      SMALLINT NOT NULL,
  total_size  BIGINT NOT NULL DEFAULT 0 CHECK (total_size >= 0),
  stored_size BIGINT NOT NULL DEFAULT 0 CHECK (stored_size >= 0 AND stored_size <= total_size),
  path        TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_file_status ON file(status);

-- resource_file link table
CREATE TABLE IF NOT EXISTS resource_file (
  resource_id TEXT  NOT NULL REFERENCES resource(resource_id) ON DELETE CASCADE,
  file_hash   text NOT NULL REFERENCES file(hash) ON DELETE CASCADE,
  path        TEXT  NOT NULL,
  PRIMARY KEY (resource_id, file_hash, path)
);

CREATE INDEX IF NOT EXISTS idx_resource_file_resource ON resource_file(resource_id);
CREATE INDEX IF NOT EXISTS idx_resource_file_file ON resource_file(file_hash);

-- Triggers to maintain updated_at
DROP TRIGGER IF EXISTS trg_resource_set_updated_at ON resource;
CREATE TRIGGER trg_resource_set_updated_at
BEFORE UPDATE ON resource
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_file_set_updated_at ON file;
CREATE TRIGGER trg_file_set_updated_at
BEFORE UPDATE ON file
FOR EACH ROW EXECUTE FUNCTION set_updated_at();
