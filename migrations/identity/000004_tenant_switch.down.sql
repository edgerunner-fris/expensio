BEGIN;

DROP POLICY IF EXISTS self_membership_read ON app.memberships;

COMMIT;
