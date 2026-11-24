ALTER TABLE ai_profiles ADD COLUMN instructions TEXT NOT NULL DEFAULT '' CHECK (length(instructions) <= 8192);
ALTER TABLE ai_profiles ADD COLUMN headers_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(headers_json) AND json_type(headers_json) = 'object');
