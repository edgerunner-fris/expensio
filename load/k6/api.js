// Expensio 基线压测。
//
// 目标不是把系统打满，而是用可重复的负载验证三件事：
//   1. 网关 → identity / expense / report / notification 的同步链路稳定；
//   2. 每次提交/取消都会经过 outbox → Kafka → 报表/通知的异步链路；
//   3. Prometheus 上的请求量、P95、5xx 与异步失败率与压测结果一致。
//
// 单租户 API 限流是 1200 次/分钟：3 VU × 每轮约 7 个请求、间隔 1.5s
// 约为 840 次/分钟，留出重试余量。增大 VU 前先调整 EXPENSIO_RATE_LIMIT_*，
// 否则测到的是 429 而不是容量。
import http from 'k6/http';
import { check, sleep } from 'k6';

const BASE = __ENV.BASE_URL || 'http://gateway:8080';
const PASSWORD = 'Load-Passw0rd!';

export const options = {
  scenarios: {
    mixed: {
      executor: 'constant-vus',
      vus: 3,
      duration: __ENV.DURATION || '30s',
      gracefulStop: '10s',
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<500'],
    checks: ['rate>0.99'],
  },
};

function apiParams(token) {
  return {
    headers: {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/json',
      Accept: 'application/json',
    },
  };
}

function withTag(params, name) {
  return {
    headers: params.headers,
    tags: { name },
  };
}

function json(res) {
  try {
    return JSON.parse(res.body);
  } catch (e) {
    return null;
  }
}

export function setup() {
  const suffix = `${Date.now()}`;
  const slug = `k6-${suffix}`;
  const email = `load-${suffix}@example.com`;

  const reg = http.post(
    `${BASE}/api/v1/auth/register`,
    JSON.stringify({
      tenant_slug: slug,
      tenant_name: `k6 tenant ${suffix}`,
      email,
      password: PASSWORD,
      display_name: 'Load Owner',
    }),
    { headers: { 'Content-Type': 'application/json' }, tags: { name: 'setup_register' } },
  );
  if (!check(reg, { 'setup register 201': (r) => r.status === 201 })) {
    throw new Error(`register failed: ${reg.status} ${reg.body}`);
  }
  const regBody = json(reg);
  const token = regBody.data.access_token;

  const now = new Date();
  const start = new Date(now.getFullYear(), now.getMonth(), 1).toISOString().slice(0, 10);
  const end = new Date(now.getFullYear(), now.getMonth() + 1, 0).toISOString().slice(0, 10);

  const budget = http.post(
    `${BASE}/api/v1/budgets`,
    JSON.stringify({
      name: `k6 budget ${suffix}`,
      category: 'travel',
      currency: 'USD',
      period_start: start,
      period_end: end,
      amount_minor: 500000000,
    }),
    withTag(apiParams(token), 'setup_budget'),
  );
  if (!check(budget, { 'setup budget 201': (r) => r.status === 201 })) {
    throw new Error(`budget failed: ${budget.status} ${budget.body}`);
  }

  return {
    token,
    slug,
    from: start,
    to: end,
  };
}

export default function (data) {
  const p = apiParams(data.token);
  const unique = `${__VU}-${__ITER}-${Date.now()}`;
  const occurredAt = new Date().toISOString();

  const created = http.post(
    `${BASE}/api/v1/expenses`,
    JSON.stringify({
      category: 'travel',
      currency: 'USD',
      amount_minor: 1234,
      merchant: `k6-${unique}`,
      description: 'load test draft',
      occurred_at: occurredAt,
    }),
    withTag(p, 'create_expense'),
  );
  const createdBody = json(created);
  const okCreate =
    check(created, { 'create 201': (r) => r.status === 201 }) &&
    createdBody &&
    createdBody.data &&
    createdBody.data.id;

  if (okCreate) {
    const id = createdBody.data.id;

    const submitted = http.post(
      `${BASE}/api/v1/expenses/${id}/submit`,
      JSON.stringify({}),
      withTag(p, 'submit_expense'),
    );
    check(submitted, { 'submit 200': (r) => r.status === 200 });

    // 不审批（自审批被禁止），提交后立即由本人取消：状态机与释放预算
    // 都会走一遍，同时产生 submitted / canceled 两条事件。
    const canceled = http.post(
      `${BASE}/api/v1/expenses/${id}/cancel`,
      JSON.stringify({ comment: 'load test cleanup' }),
      withTag(p, 'cancel_expense'),
    );
    check(canceled, { 'cancel 200': (r) => r.status === 200 });
  }

  const list = http.get(`${BASE}/api/v1/expenses?page=1&page_size=10`, withTag(p, 'list_expenses'));
  check(list, { 'list 200': (r) => r.status === 200 });

  const summary = http.get(
    `${BASE}/api/v1/reports/expenses/summary?scope=all&from=${data.from}&to=${data.to}`,
    withTag(p, 'report_summary'),
  );
  check(summary, { 'summary 200': (r) => r.status === 200 });

  const notifications = http.get(
    `${BASE}/api/v1/notifications?page=1&page_size=20`,
    withTag(p, 'list_notifications'),
  );
  check(notifications, { 'notifications 200': (r) => r.status === 200 });

  // 1.5s 的思考时间让单租户速率稳定在限流阈值以内；
  // 真正要测吞吐上限时应另建场景并同步提高 rate_limit。
  sleep(1.5);
}
