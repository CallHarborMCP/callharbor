-- Recording metadata. The encrypted file keeps its own plaintext header (sample
-- rate, channels, salt); these columns mirror what the API reports and what
-- the crash sweep and the audio reader need without opening the file.
--
-- sample_rate is the leg sample rate the WAV was written at. It is NULL until
-- the writer has fixed the format, so a 'recording' row with no audio yet
-- remains valid. channels is always 2: channel 0 is the remote party and
-- channel 1 is the agent audio that was played.
--
-- quality_flags is the JSON array of persisted degradation flags. Each value is
-- drawn from a fixed vocabulary defined in internal/recording, so the column
-- only checks shape here.
ALTER TABLE recordings ADD COLUMN sample_rate INTEGER CHECK (sample_rate IS NULL OR sample_rate BETWEEN 8000 AND 48000);
ALTER TABLE recordings ADD COLUMN channels INTEGER NOT NULL DEFAULT 2 CHECK (channels = 2);
ALTER TABLE recordings ADD COLUMN quality_flags TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(quality_flags) AND json_type(quality_flags) = 'array');
