ALTER TABLE transcript_turns DROP COLUMN quality_json;
ALTER TABLE transcript_turns DROP COLUMN played_is_estimate;
ALTER TABLE transcript_turns DROP COLUMN played_ms;
ALTER TABLE transcript_turns DROP COLUMN generated_ms;
ALTER TABLE transcript_turns DROP COLUMN correlation_id;
ALTER TABLE transcript_turns DROP COLUMN source_ids_json;
ALTER TABLE transcript_turns DROP COLUMN timing_source;
ALTER TABLE transcript_turns DROP COLUMN fidelity;
