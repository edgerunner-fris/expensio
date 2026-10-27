-- 回滚 000005：先删依赖 expenses 的新表与触发器，再删新增列。
-- 000002 的函数与约束保持原样，因为本迁移没有替换它们。
BEGIN;

DROP TABLE IF EXISTS app.expense_attachments;
DROP TABLE IF EXISTS app.expense_comments;
DROP TABLE IF EXISTS app.expense_approvals;
DROP TABLE IF EXISTS app.approval_policies;

DROP TRIGGER IF EXISTS expenses_guard_approval_progress ON app.expenses;
DROP FUNCTION IF EXISTS app.expenses_guard_approval_progress();
-- 删表会带走触发器，但不会带走触发器函数；不删掉它，重新 up 会撞名。
DROP FUNCTION IF EXISTS app.expense_approvals_guard();

ALTER TABLE app.expenses
    DROP CONSTRAINT IF EXISTS expenses_approval_level_consistency,
    DROP CONSTRAINT IF EXISTS expenses_approval_level_check,
    DROP CONSTRAINT IF EXISTS expenses_required_levels_check,
    DROP COLUMN IF EXISTS approval_level,
    DROP COLUMN IF EXISTS required_approval_levels;

COMMIT;
