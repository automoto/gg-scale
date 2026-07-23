-- Irreversible: the duplicate queued tickets cancelled by the up migration are
-- not restored. The unique index is dropped by the paired index migration.
SELECT 1;
