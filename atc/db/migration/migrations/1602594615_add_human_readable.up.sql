BEGIN;
    ALTER TABLE resources ADD COLUMN human_readable text;
    ALTER TABLE jobs ADD COLUMN human_readable text;
COMMIT;
