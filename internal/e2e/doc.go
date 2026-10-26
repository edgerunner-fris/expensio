// Package e2e 存放跨服务的事件链路测试。
//
// 这里不使用任何假实现：费用接口、outbox 投递器、Kafka、报表消费者与
// 报表查询 API 全部是生产代码，依赖真实 PostgreSQL 与 Kafka 容器。
// 单独成包是因为它同时依赖 expense 与 report，放在任一服务的测试包里
// 都会把"跨服务"变成"某个服务的内部测试"。
package e2e
