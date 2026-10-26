BEGIN;

ALTER DEFAULT PRIVILEGES FOR ROLE notification_migrator IN SCHEMA app
  REVOKE ALL ON SEQUENCES FROM notification_app;
ALTER DEFAULT PRIVILEGES FOR ROLE notification_migrator IN SCHEMA app
  REVOKE ALL ON TABLES FROM notification_app;

DO $do$
BEGIN
  EXECUTE format('ALTER DATABASE %I RESET search_path', current_database());
END
$do$;

DROP SCHEMA IF EXISTS app CASCADE;

COMMIT;
