-- 月度续费：计费锚点日、跨租户到期目录与"只向前滚动"的周期变更。
-- 设计见 ADR-0014。
--
-- 为什么要多一张目录表：subscriptions 是 FORCE RLS，续费任务需要跨租户
-- 找出"今天到期"的订阅，而 RLS 下任何非租户上下文都读不到行。方案与
-- invoice_lookup 相同：由订阅表的触发器在同事务维护一张无 RLS 的目录，
-- 运行时角色对目录没有直接权限，只能通过 SECURITY DEFINER 函数读到到期行。
--
-- 注意 FORCE 对表属主同样生效（ADR-0013 的教训）：下面的回填必须临时
-- 关掉 FORCE，否则迁移自己 UPDATE/SELECT 都是 0 行。DDL 在事务内，
-- 这段窗口对其他会话不可见。
BEGIN;

-- 1. 计费锚点日 ------------------------------------------------------------
--
-- 每月 31 日开通的订阅，2 月只能结束于 27 日；没有锚点日，3 月会从 28 日开始，
-- 日期逐月漂移。anchor_day 在插入时由触发器取首次周期开始日，之后不可变。
ALTER TABLE app.subscriptions ADD COLUMN anchor_day smallint;

ALTER TABLE app.subscriptions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE app.subscriptions DISABLE TRIGGER subscriptions_guard_update;

UPDATE app.subscriptions
SET anchor_day = EXTRACT(DAY FROM current_period_start)::smallint;

-- 2. 到期目录 --------------------------------------------------------------
CREATE TABLE app.renewal_schedule (
    subscription_id    uuid PRIMARY KEY,
    tenant_id          uuid NOT NULL,
    current_period_end date NOT NULL,
    status             text NOT NULL,
    CONSTRAINT renewal_schedule_status_check CHECK (
        status IN ('active', 'past_due', 'canceled')
    )
);

CREATE INDEX renewal_schedule_due_idx
    ON app.renewal_schedule (current_period_end)
    WHERE status = 'active';

-- 目录由触发器维护，运行时角色不得直接读写：其中的 tenant_id 是跨租户数据，
-- 直接授予 SELECT 等于给了一个绕过 RLS 的旁路。
REVOKE ALL ON app.renewal_schedule FROM billing_app;

INSERT INTO app.renewal_schedule (subscription_id, tenant_id, current_period_end, status)
SELECT id, tenant_id, current_period_end, status FROM app.subscriptions;

ALTER TABLE app.subscriptions ENABLE TRIGGER subscriptions_guard_update;
ALTER TABLE app.subscriptions FORCE ROW LEVEL SECURITY;

ALTER TABLE app.subscriptions ALTER COLUMN anchor_day SET NOT NULL;
ALTER TABLE app.subscriptions
    ADD CONSTRAINT subscriptions_anchor_day_check CHECK (anchor_day BETWEEN 1 AND 31);

-- 锚点日不接受调用方传入：它完全由首次周期开始日决定，避免"锚点与周期不一致"。
CREATE FUNCTION app.subscriptions_fill_anchor_day() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    NEW.anchor_day := EXTRACT(DAY FROM NEW.current_period_start)::smallint;
    RETURN NEW;
END
$$;

CREATE TRIGGER subscriptions_fill_anchor_day BEFORE INSERT ON app.subscriptions
    FOR EACH ROW EXECUTE FUNCTION app.subscriptions_fill_anchor_day();

-- 3. 周期只能整体向前滚动一个周期 ------------------------------------------
--
-- 旧版本把 current_period_start/end 视为完全不可变，续费无法落库。
-- 这里放开一个精确的形状：新开始日必须紧接旧结束日、两列必须一起变、
-- 状态必须保持 active（续费不改变订阅状态）。周期长度是否等于套餐周期
-- 由服务层按 plans.interval 计算，数据库只负责"不能倒退、不能跳空"。
CREATE OR REPLACE FUNCTION app.subscriptions_guard_update() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    IF NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.plan_code <> OLD.plan_code OR NEW.seats <> OLD.seats
       OR NEW.anchor_day <> OLD.anchor_day
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'subscription identity columns are immutable'
            USING ERRCODE = '23514';
    END IF;

    IF NEW.current_period_start IS DISTINCT FROM OLD.current_period_start
       OR NEW.current_period_end IS DISTINCT FROM OLD.current_period_end THEN
        IF NEW.current_period_start IS DISTINCT FROM OLD.current_period_end + 1
           OR NEW.current_period_start <= OLD.current_period_start
           OR NEW.current_period_end <= OLD.current_period_end
           OR NEW.status IS DISTINCT FROM OLD.status
           OR NEW.status <> 'active' THEN
            RAISE EXCEPTION 'subscription periods may only roll forward contiguously'
                USING ERRCODE = '23514';
        END IF;
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

CREATE FUNCTION app.subscriptions_record_schedule() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, app AS
$$
BEGIN
    INSERT INTO app.renewal_schedule (subscription_id, tenant_id, current_period_end, status)
    VALUES (NEW.id, NEW.tenant_id, NEW.current_period_end, NEW.status)
    ON CONFLICT (subscription_id) DO UPDATE
        SET tenant_id          = EXCLUDED.tenant_id,
            current_period_end = EXCLUDED.current_period_end,
            status             = EXCLUDED.status;
    RETURN NEW;
END
$$;

REVOKE ALL ON FUNCTION app.subscriptions_record_schedule() FROM PUBLIC;

CREATE TRIGGER subscriptions_record_schedule AFTER INSERT OR UPDATE ON app.subscriptions
    FOR EACH ROW EXECUTE FUNCTION app.subscriptions_record_schedule();

-- 4. 到期查询：唯一的跨租户读入口 -----------------------------------------
--
-- 只返回订阅 ID、租户 ID 与结束日，不暴露套餐、席位等业务列。续费事务仍然
-- 在 WithinTenantID 里重新加锁并复核状态，目录只用于"找到候选"。
CREATE FUNCTION app.due_subscriptions(as_of date, max_rows integer DEFAULT 100)
    RETURNS TABLE (subscription_id uuid, tenant_id uuid, current_period_end date)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, app AS
$$
    SELECT s.subscription_id, s.tenant_id, s.current_period_end
    FROM app.renewal_schedule s
    WHERE s.status = 'active' AND s.current_period_end < as_of
    ORDER BY s.current_period_end, s.subscription_id
    LIMIT GREATEST(1, max_rows)
$$;

REVOKE ALL ON FUNCTION app.due_subscriptions(date, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.due_subscriptions(date, integer) TO billing_app;

COMMIT;
