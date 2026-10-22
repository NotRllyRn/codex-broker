-- Keep the managed credential and its history; exports cannot be renewed safely
-- as independent credentials from the same OAuth grant.
DELETE FROM credential_bundles WHERE state='EXPORT';
DROP INDEX IF EXISTS credential_one_export_uq;
