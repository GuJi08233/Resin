ALTER TABLE nodes_dynamic
ADD COLUMN ipinfo_json TEXT NOT NULL DEFAULT '';

ALTER TABLE nodes_dynamic
ADD COLUMN last_ipinfo_probe_attempt_ns INTEGER NOT NULL DEFAULT 0;
