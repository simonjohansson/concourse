BEGIN;
    ALTER TABLE resources ADD COLUMN human_readable text;
COMMIT;
