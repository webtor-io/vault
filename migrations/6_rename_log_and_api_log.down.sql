DROP TABLE IF EXISTS api_log;

ALTER TABLE worker_log RENAME TO log;

ALTER INDEX idx_worker_log_resource RENAME TO idx_log_resource;
ALTER INDEX idx_worker_log_type RENAME TO idx_log_type;
ALTER INDEX idx_worker_log_started_at RENAME TO idx_log_started_at;
ALTER INDEX idx_worker_log_finished_at RENAME TO idx_log_finished_at;
ALTER INDEX idx_worker_log_status RENAME TO idx_log_status;
