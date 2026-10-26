-- 回滚 expense 核心表。
-- btree_gist 是有意保留的：扩展可能被同一库里的其他对象使用，
-- 迁移回滚不应该替别人做决定。
BEGIN;

DROP TRIGGER IF EXISTS budgets_guard_update ON app.budgets;
DROP TRIGGER IF EXISTS expenses_guard_update ON app.expenses;
DROP TRIGGER IF EXISTS expenses_guard_insert ON app.expenses;
DROP TRIGGER IF EXISTS expenses_touch ON app.expenses;
DROP TRIGGER IF EXISTS budgets_touch ON app.budgets;

DROP FUNCTION IF EXISTS app.budgets_guard_update();
DROP FUNCTION IF EXISTS app.expenses_guard_update();
DROP FUNCTION IF EXISTS app.expenses_guard_insert();
DROP FUNCTION IF EXISTS app.touch_updated_at();

DROP TABLE IF EXISTS app.approval_steps;
DROP TABLE IF EXISTS app.expenses;
DROP TABLE IF EXISTS app.expense_status_transitions;
DROP TABLE IF EXISTS app.budgets;

COMMIT;
