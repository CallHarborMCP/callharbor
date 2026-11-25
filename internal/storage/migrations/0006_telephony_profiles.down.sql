DROP INDEX IF EXISTS telephony_profile_grants_grantee_idx;
DROP TABLE IF EXISTS telephony_profile_grants;
ALTER TABLE telephony_profiles DROP COLUMN connectivity_checked_at;
ALTER TABLE telephony_profiles DROP COLUMN connectivity_reason;
ALTER TABLE telephony_profiles DROP COLUMN connectivity_status;
