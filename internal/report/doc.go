// Package report 维护费用报表的读模型。
//
// 它只消费事件、写自己的数据库，不调用 expense 的 API，也不跨库查询。
// 设计见 docs/adr/0004-event-driven-read-model.md。
package report
