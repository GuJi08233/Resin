ALTER TABLE platforms
ADD COLUMN continent_filters_json TEXT NOT NULL DEFAULT '[]';

ALTER TABLE platforms
ADD COLUMN latency_filters_json TEXT NOT NULL DEFAULT '{}';
