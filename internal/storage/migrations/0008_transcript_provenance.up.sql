-- Transcript provenance. Each revision of a turn records how trustworthy its
-- text is relative to what was heard (fidelity), where its timing came from,
-- the provider identifiers it was built from, and the played/generated agent
-- audio it was reconciled with. Rows stay immutable: a change is a new revision.
ALTER TABLE transcript_turns ADD COLUMN fidelity TEXT NOT NULL DEFAULT 'unavailable' CHECK (fidelity IN (
    'played', 'interrupted_text_includes_unplayed', 'playout_unconfirmed', 'not_played',
    'provider_asr_approximate', 'unavailable'));
ALTER TABLE transcript_turns ADD COLUMN timing_source TEXT NOT NULL DEFAULT 'local_receive' CHECK (timing_source IN ('provider', 'local_receive'));
ALTER TABLE transcript_turns ADD COLUMN source_ids_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(source_ids_json) AND json_type(source_ids_json) = 'array');
ALTER TABLE transcript_turns ADD COLUMN correlation_id TEXT CHECK (correlation_id IS NULL OR length(correlation_id) BETWEEN 1 AND 128);
ALTER TABLE transcript_turns ADD COLUMN generated_ms INTEGER CHECK (generated_ms IS NULL OR generated_ms >= 0);
ALTER TABLE transcript_turns ADD COLUMN played_ms INTEGER CHECK (played_ms IS NULL OR played_ms >= 0);
ALTER TABLE transcript_turns ADD COLUMN played_is_estimate INTEGER NOT NULL DEFAULT 0 CHECK (played_is_estimate IN (0, 1));
ALTER TABLE transcript_turns ADD COLUMN quality_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(quality_json) AND json_type(quality_json) = 'array');
