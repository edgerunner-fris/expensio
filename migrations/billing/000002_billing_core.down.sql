-- 回滚 billing 核心表。先删表（连带触发器），再删函数，
-- 最后删被函数引用的 invoice_lookup 与套餐目录。
BEGIN;

DROP TABLE IF EXISTS app.outbox_events;
DROP TABLE IF EXISTS app.webhook_events;
DROP TABLE IF EXISTS app.payments;
DROP TABLE IF EXISTS app.invoices;
DROP TABLE IF EXISTS app.subscriptions;
DROP TABLE IF EXISTS app.subscription_status_transitions;

DROP FUNCTION IF EXISTS app.invoice_tenant(uuid);
DROP FUNCTION IF EXISTS app.invoices_record_lookup();
DROP FUNCTION IF EXISTS app.invoices_guard_update();
DROP FUNCTION IF EXISTS app.subscriptions_guard_update();
DROP FUNCTION IF EXISTS app.touch_updated_at();

DROP TABLE IF EXISTS app.invoice_lookup;
DROP TABLE IF EXISTS app.plans;

COMMIT;
