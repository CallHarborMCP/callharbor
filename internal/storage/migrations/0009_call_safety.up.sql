-- Call safety. Per-user call policy overrides an administrator sets. Each
-- numeric column is NULL to inherit the installation value; a value can only
-- tighten it, which the service enforces. Prefix lists are JSON arrays of
-- E.164 prefixes. Also records the cost of each call: a pre-dial estimate or a
-- provider-reported charge, never both as the same status.
CREATE TABLE user_call_policies (
    user_id TEXT PRIMARY KEY REFERENCES users (id) ON DELETE RESTRICT,
    max_concurrent_calls INTEGER CHECK (max_concurrent_calls IS NULL OR max_concurrent_calls BETWEEN 1 AND 1000),
    max_calls_per_hour INTEGER CHECK (max_calls_per_hour IS NULL OR max_calls_per_hour BETWEEN 1 AND 10000),
    max_duration_seconds INTEGER CHECK (max_duration_seconds IS NULL OR max_duration_seconds BETWEEN 30 AND 7200),
    max_cost_micros INTEGER CHECK (max_cost_micros IS NULL OR max_cost_micros > 0),
    allow_prefixes_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(allow_prefixes_json) AND json_type(allow_prefixes_json) = 'array'),
    deny_prefixes_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(deny_prefixes_json) AND json_type(deny_prefixes_json) = 'array'),
    updated_at TEXT NOT NULL,
    updated_by TEXT REFERENCES users (id) ON DELETE RESTRICT
);

ALTER TABLE calls ADD COLUMN cost_status TEXT CHECK (cost_status IS NULL OR cost_status IN ('known', 'estimated', 'unavailable'));
ALTER TABLE calls ADD COLUMN cost_amount_micros INTEGER CHECK (cost_amount_micros IS NULL OR cost_amount_micros >= 0);
ALTER TABLE calls ADD COLUMN cost_currency TEXT CHECK (cost_currency IS NULL OR length(cost_currency) = 3);

-- The provider's reported cost, from call.cost. It is stored apart from the
-- estimate above, so both can be shown. provider_cost_status is 'success' or
-- 'error' as Telnyx reports it. Amount and currency are set only for a success
-- whose parts agree on one currency.
ALTER TABLE calls ADD COLUMN provider_cost_status TEXT CHECK (provider_cost_status IS NULL OR provider_cost_status IN ('success', 'error'));
ALTER TABLE calls ADD COLUMN provider_cost_amount_micros INTEGER CHECK (provider_cost_amount_micros IS NULL OR provider_cost_amount_micros >= 0);
ALTER TABLE calls ADD COLUMN provider_cost_currency TEXT CHECK (provider_cost_currency IS NULL OR length(provider_cost_currency) = 3);
ALTER TABLE calls ADD COLUMN provider_cost_event_id TEXT;

-- Reconciliation marker. hangup_requested_at is the last time CallHarbor sent
-- a hangup for the call's leg, and hangup_attempts counts those sends. The
-- reconciliation sweep reads them to decide when to query or resend. Both are
-- NULL or zero until the first hangup is sent.
ALTER TABLE calls ADD COLUMN hangup_requested_at TEXT;
ALTER TABLE calls ADD COLUMN hangup_attempts INTEGER NOT NULL DEFAULT 0 CHECK (hangup_attempts >= 0);
