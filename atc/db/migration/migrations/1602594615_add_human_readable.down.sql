BEGIN;
    ALTER TABLE resources DROP COLUMN human_readable;
    ALTER TABLE jobs DROP COLUMN human_readable;
COMMIT;
