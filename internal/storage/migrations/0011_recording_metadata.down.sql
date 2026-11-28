-- Restores the 0010 schema. Recording metadata columns are dropped; recording
-- rows themselves are kept, so the encrypted files they point at stay reachable
-- under the 0001 columns.
ALTER TABLE recordings DROP COLUMN quality_flags;
ALTER TABLE recordings DROP COLUMN channels;
ALTER TABLE recordings DROP COLUMN sample_rate;
