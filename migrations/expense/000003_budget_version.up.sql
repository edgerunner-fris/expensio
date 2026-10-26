-- 预算增加乐观锁版本号。
--
-- 000002 已经把所有金额不变量放在数据库里；这里补的是并发编辑语义：
-- 两个管理员同时改同一条预算时，后提交的一方必须失败，而不是静默覆盖。
-- 版本号由触发器自增，任何 UPDATE 路径（包括手工修数）都不会漏掉。
BEGIN;

ALTER TABLE app.budgets ADD COLUMN version integer NOT NULL DEFAULT 1;
ALTER TABLE app.budgets
    ADD CONSTRAINT budgets_version_check CHECK (version > 0);

-- 在 000002 的函数体上追加 version 自增；其余校验保持原样。
CREATE OR REPLACE FUNCTION app.budgets_guard_update() RETURNS trigger
    LANGUAGE plpgsql AS
$$
BEGIN
    IF NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'budget identity columns are immutable'
            USING ERRCODE = '23514';
    END IF;

    IF OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL THEN
        RAISE EXCEPTION 'budget % is soft-deleted and cannot be restored', OLD.id
            USING ERRCODE = '23514';
    END IF;

    -- 还有占用或支出时不允许软删，否则报表会出现"花掉的钱没有预算"。
    IF NEW.deleted_at IS NOT NULL AND OLD.deleted_at IS NULL
       AND (OLD.reserved_minor > 0 OR OLD.spent_minor > 0) THEN
        RAISE EXCEPTION 'budget % still has committed money', OLD.id
            USING ERRCODE = '23514';
    END IF;

    NEW.version := OLD.version + 1;
    RETURN NEW;
END
$$;

COMMIT;
