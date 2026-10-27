BEGIN;

DROP FUNCTION IF EXISTS app.find_invitation(bytea);
DROP TABLE IF EXISTS app.invitations;

COMMIT;
