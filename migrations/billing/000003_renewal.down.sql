-- 回滚续费：目录、触发器与锚点日全部撤销，guard 恢复为"周期完全不可变"。
-- 已经续费过的订阅会保留前进后的周期；回滚不会把它改回去。
BEGIN;

DROP FUNCTION app.due_subscriptions(date, integer);

DROP TRIGGER subscriptions_record_schedule ON app.subscriptions;
DROP FUNCTION app.subscriptions_record_schedule();
DROP TABLE app.renewal_schedule;

-- 恢复 M4 的原始 guard：周期列完全不可变。
CREATE OR REPLACE FUNCTION app.subscriptions_guard_update() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    IF NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.plan_code <> OLD.plan_code OR NEW.seats <> OLD.seats
       OR NEW.current_period_start <> OLD.current_period_start
       OR NEW.current_period_end <> OLD.current_period_end
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'subscription identity columns are immutable'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status AND NOT EXISTS (
        SELECT 1 FROM app.subscription_status_transitions t
        WHERE t.from_status = OLD.status AND t.to_status = NEW.status
    ) THEN
        RAISE EXCEPTION 'invalid subscription status transition % -> %', OLD.status, NEW.status
            USING ERRCODE = '23514';
    END IF;

    IF NEW.status <> 'canceled' AND NEW.canceled_at IS NOT NULL THEN
        RAISE EXCEPTION 'canceled_at is only allowed on canceled subscriptions'
            USING ERRCODE = '23514';
    END IF;

    NEW.version := OLD.version + 1;
    RETURN NEW;
END
$$;

DROP TRIGGER subscriptions_fill_anchor_day ON app.subscriptions;
DROP FUNCTION app.subscriptions_fill_anchor_day();

ALTER TABLE app.subscriptions DROP CONSTRAINT subscriptions_anchor_day_check;
ALTER TABLE app.subscriptions DROP COLUMN anchor_day;

COMMIT;
