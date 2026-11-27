-- Restores the 0009 schema. The triggers are dropped first, because SQLite
-- refuses to drop a column that a trigger references. Recorded task outcomes
-- are discarded with their columns; calls themselves are kept.
DROP INDEX calls_outcome_pending_idx;
DROP TRIGGER calls_task_outcome_write_once;
DROP TRIGGER calls_task_outcome_consistent_update;
DROP TRIGGER calls_task_outcome_consistent_insert;
ALTER TABLE calls DROP COLUMN outcome_assessed_at;
ALTER TABLE calls DROP COLUMN outcome_source;
ALTER TABLE calls DROP COLUMN outcome_evidence;
ALTER TABLE calls DROP COLUMN outcome_reason_codes;
