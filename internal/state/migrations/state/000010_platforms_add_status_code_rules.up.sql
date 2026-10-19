ALTER TABLE platforms
ADD COLUMN status_code_rules_json TEXT NOT NULL DEFAULT '[]';
