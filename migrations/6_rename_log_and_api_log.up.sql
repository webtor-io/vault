-- Rename log -> worker_log
ALTER TABLE log RENAME TO worker_log;

ALTER INDEX idx_log_resource RENAME TO idx_worker_log_resource;
ALTER INDEX idx_log_type RENAME TO idx_worker_log_type;
ALTER INDEX idx_log_started_at RENAME TO idx_worker_log_started_at;
ALTER INDEX idx_log_finished_at RENAME TO idx_worker_log_finished_at;
ALTER INDEX idx_log_status RENAME TO idx_worker_log_status;

-- Create api_log table
CREATE TABLE IF NOT EXISTS api_log (
  api_log_id     uuid DEFAULT uuid_generate_v4() NOT NULL PRIMARY KEY,
  operation_type SMALLINT    NOT NULL, -- 0 - store, 1 - delete
  resource_id    TEXT        NOT NULL,
  http_status    SMALLINT    NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_api_log_resource ON api_log(resource_id);
CREATE INDEX IF NOT EXISTS idx_api_log_type ON api_log(operation_type);
CREATE INDEX IF NOT EXISTS idx_api_log_created_at ON api_log(created_at);
