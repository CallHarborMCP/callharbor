-- Restores the 0008 schema. Rolling back drops the cost columns and the
-- per-user policy rows; calls themselves are kept.
ALTER TABLE calls DROP COLUMN provider_cost_event_id;
ALTER TABLE calls DROP COLUMN provider_cost_currency;
ALTER TABLE calls DROP COLUMN provider_cost_amount_micros;
ALTER TABLE calls DROP COLUMN provider_cost_status;
ALTER TABLE calls DROP COLUMN hangup_attempts;
ALTER TABLE calls DROP COLUMN hangup_requested_at;
ALTER TABLE calls DROP COLUMN cost_currency;
ALTER TABLE calls DROP COLUMN cost_amount_micros;
ALTER TABLE calls DROP COLUMN cost_status;
DROP TABLE user_call_policies;
