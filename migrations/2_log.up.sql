CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS log (
  log_id         uuid DEFAULT uuid_generate_v4() NOT NULL,
  operation_type SMALLINT    NOT NULL, -- 0 - store, 1 - delete
  started_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  finished_at    TIMESTAMPTZ,
  resource_id    TEXT        NOT NULL,
  status         SMALLINT,              -- 0 - success, 1 - fail
  error_text     TEXT                   -- optional error description on failure
);

CREATE INDEX IF NOT EXISTS idx_log_resource ON log(resource_id);
CREATE INDEX IF NOT EXISTS idx_log_type ON log(operation_type);
CREATE INDEX IF NOT EXISTS idx_log_started_at ON log(started_at);
CREATE INDEX IF NOT EXISTS idx_log_finished_at ON log(finished_at);
CREATE INDEX IF NOT EXISTS idx_log_status ON log(status);
