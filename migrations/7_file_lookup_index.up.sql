CREATE INDEX IF NOT EXISTS idx_file_status_total_size ON file (status, total_size);
CREATE INDEX IF NOT EXISTS idx_resource_file_path ON resource_file (path);
