-- 022_e2ee_prekey_lengths.sql — enforce bundle key sizes for databases that
-- already applied migration 021 before its inline checks were added.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'e2ee_prekey_bundle_identity_size'
                     AND conrelid = 'e2ee_prekey_bundles'::regclass) THEN
        ALTER TABLE e2ee_prekey_bundles
            ADD CONSTRAINT e2ee_prekey_bundle_identity_size
            CHECK (octet_length(identity_dh) = 32) NOT VALID;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'e2ee_prekey_bundle_signed_size'
                     AND conrelid = 'e2ee_prekey_bundles'::regclass) THEN
        ALTER TABLE e2ee_prekey_bundles
            ADD CONSTRAINT e2ee_prekey_bundle_signed_size
            CHECK (octet_length(signed_prekey) = 32) NOT VALID;
    END IF;
END $$;

ALTER TABLE e2ee_prekey_bundles VALIDATE CONSTRAINT e2ee_prekey_bundle_identity_size;
ALTER TABLE e2ee_prekey_bundles VALIDATE CONSTRAINT e2ee_prekey_bundle_signed_size;
