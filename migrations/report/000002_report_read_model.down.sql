-- 回滚 000002：删除读模型。事实表与幂等表都可由事件重建，直接丢弃。
BEGIN;

DROP TABLE app.expense_facts;
DROP TABLE app.processed_events;

COMMIT;
