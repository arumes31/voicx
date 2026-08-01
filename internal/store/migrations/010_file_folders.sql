-- 010_file_folders.sql — wave 7 files & media.
--
-- - Virtual folders: files.folder ('' = channel root). Folders are derived
--   from file rows — empty folders do not persist (documented behavior).
-- - Uniqueness widens from (channel_id, name) to (channel_id, folder, name)
--   so the same file name can exist in different folders.
-- Idempotent: safe to re-run via Store.Migrate().

ALTER TABLE files ADD COLUMN IF NOT EXISTS folder TEXT NOT NULL DEFAULT '';

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid
        WHERE t.relname = 'files' AND c.contype = 'u' AND c.conname = 'files_channel_id_name_key'
    ) THEN
        ALTER TABLE files DROP CONSTRAINT files_channel_id_name_key;
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_files_channel_folder_name ON files (channel_id, folder, name);
