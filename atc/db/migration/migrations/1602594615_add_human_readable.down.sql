BEGIN;
    ALTER TABLE resources DROP COLUMN human_readable;
COMMIT;
