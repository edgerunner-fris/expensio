BEGIN;

DROP FUNCTION IF EXISTS app.claim_emails(integer);
DROP TABLE IF EXISTS app.email_outbox;
DROP TABLE IF EXISTS app.notifications;
DROP TABLE IF EXISTS app.processed_events;

COMMIT;
