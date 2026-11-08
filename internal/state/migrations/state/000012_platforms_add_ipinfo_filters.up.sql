ALTER TABLE platforms
ADD COLUMN ipinfo_filters_json TEXT NOT NULL DEFAULT '{}';
