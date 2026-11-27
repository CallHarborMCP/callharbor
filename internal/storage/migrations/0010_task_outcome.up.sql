-- Task outcome. The evaluated task result is stored apart from the technical
-- status and from calls.outcome_reason_code, which keeps its technical meaning.
-- The four supporting columns and outcome itself are set together, once. The
-- triggers below enforce both rules, because SQLite cannot add a table CHECK
-- that spans columns with ALTER TABLE.
ALTER TABLE calls ADD COLUMN outcome_reason_codes TEXT CHECK (outcome_reason_codes IS NULL OR (json_valid(outcome_reason_codes) AND json_type(outcome_reason_codes) = 'array'));
ALTER TABLE calls ADD COLUMN outcome_evidence TEXT CHECK (outcome_evidence IS NULL OR (json_valid(outcome_evidence) AND json_type(outcome_evidence) = 'array'));
ALTER TABLE calls ADD COLUMN outcome_source TEXT CHECK (outcome_source IS NULL OR outcome_source IN ('technical', 'voice_session'));
ALTER TABLE calls ADD COLUMN outcome_assessed_at TEXT;

-- Readiness sweep for calls still waiting for an outcome. Partial, so it stays
-- small: it indexes only unassessed calls.
CREATE INDEX calls_outcome_pending_idx ON calls (ended_at, id) WHERE outcome IS NULL;

-- All five fields are NULL together or set together.
CREATE TRIGGER calls_task_outcome_consistent_insert
BEFORE INSERT ON calls
WHEN ((NEW.outcome IS NULL) <> (NEW.outcome_reason_codes IS NULL))
    OR ((NEW.outcome IS NULL) <> (NEW.outcome_evidence IS NULL))
    OR ((NEW.outcome IS NULL) <> (NEW.outcome_source IS NULL))
    OR ((NEW.outcome IS NULL) <> (NEW.outcome_assessed_at IS NULL))
BEGIN
    SELECT RAISE(ABORT, 'task outcome fields must be set together');
END;

CREATE TRIGGER calls_task_outcome_consistent_update
BEFORE UPDATE OF outcome, outcome_reason_codes, outcome_evidence, outcome_source, outcome_assessed_at ON calls
WHEN ((NEW.outcome IS NULL) <> (NEW.outcome_reason_codes IS NULL))
    OR ((NEW.outcome IS NULL) <> (NEW.outcome_evidence IS NULL))
    OR ((NEW.outcome IS NULL) <> (NEW.outcome_source IS NULL))
    OR ((NEW.outcome IS NULL) <> (NEW.outcome_assessed_at IS NULL))
BEGIN
    SELECT RAISE(ABORT, 'task outcome fields must be set together');
END;

-- Once an outcome is recorded, none of the five fields can change.
CREATE TRIGGER calls_task_outcome_write_once
BEFORE UPDATE OF outcome, outcome_reason_codes, outcome_evidence, outcome_source, outcome_assessed_at ON calls
WHEN OLD.outcome IS NOT NULL AND (
    NEW.outcome IS NOT OLD.outcome
    OR NEW.outcome_reason_codes IS NOT OLD.outcome_reason_codes
    OR NEW.outcome_evidence IS NOT OLD.outcome_evidence
    OR NEW.outcome_source IS NOT OLD.outcome_source
    OR NEW.outcome_assessed_at IS NOT OLD.outcome_assessed_at)
BEGIN
    SELECT RAISE(ABORT, 'task outcome is write-once');
END;
