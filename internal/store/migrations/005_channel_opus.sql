-- 005_channel_opus.sql — per-channel Opus audio quality settings for voicx.
--
-- Adds the per-channel codec tuning used by the voice SFU (backlog 21-25):
-- the server's SDP answers carry these as Opus fmtp parameters for members of
-- the channel, and music-mode channels (stereo + high bitrate) bypass the
-- talk-power gate. Idempotent: safe to re-run via Store.Migrate().

ALTER TABLE channels
    ADD COLUMN IF NOT EXISTS opus_bitrate INTEGER NOT NULL DEFAULT 0, -- 0 = default (32000)
    ADD COLUMN IF NOT EXISTS opus_fec BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS opus_dtx BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS opus_stereo BOOLEAN NOT NULL DEFAULT FALSE;
