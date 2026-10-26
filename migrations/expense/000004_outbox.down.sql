-- 回滚 000004：删除 outbox 表。
-- 回滚后 M3 的事件链路不可用；已提交的业务数据不受影响。
BEGIN;

DROP TABLE app.outbox_events;

COMMIT;
