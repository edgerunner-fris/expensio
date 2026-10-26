-- 回滚 000003：恢复 000002 的触发器函数，再移除版本列。
-- 顺序不能反：先删列会让函数体里的 NEW.version 引用失效。
BEGIN;

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

    IF NEW.deleted_at IS NOT NULL AND OLD.deleted_at IS NULL
       AND (OLD.reserved_minor > 0 OR OLD.spent_minor > 0) THEN
        RAISE EXCEPTION 'budget % still has committed money', OLD.id
            USING ERRCODE = '23514';
    END IF;

    RETURN NEW;
END
$$;

ALTER TABLE app.budgets DROP CONSTRAINT budgets_version_check;
ALTER TABLE app.budgets DROP COLUMN version;

COMMIT;
