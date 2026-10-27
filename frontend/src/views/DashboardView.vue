<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { ECharts } from 'echarts/core'
import { apiMessage } from '@/api/client'
import { budgetApi, expenseApi } from '@/api/expenses'
import { reportApi } from '@/api/reports'
import type { Budget, Dashboard } from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import {
  categoryLabel,
  currencyExponent,
  daysAgoISO,
  formatMoney,
  percentOf,
  statusLabel,
} from '@/utils/format'

const router = useRouter()
const auth = useAuthStore()

const range = ref<[string, string]>([daysAgoISO(29), daysAgoISO(0)])
const scope = ref<'mine' | 'all'>(auth.isAdmin ? 'all' : 'mine')
const data = ref<Dashboard | null>(null)
const budgets = ref<Budget[]>([])
const inboxTotal = ref(0)
const loading = ref(false)

const palette = ['#2f6feb', '#22a06b', '#d97706', '#8b5cf6', '#e11d48', '#0ea5e9']

const totalExpenses = computed(
  () => data.value?.currency_totals.reduce((sum, t) => sum + t.expense_count, 0) ?? 0,
)

const pendingCount = computed(
  () =>
    data.value?.status_totals
      .filter((s) => s.status === 'submitted')
      .reduce((sum, s) => sum + s.expense_count, 0) ?? 0,
)

const settledCount = computed(
  () =>
    data.value?.status_totals
      .filter((s) => s.status === 'approved' || s.status === 'reimbursed')
      .reduce((sum, s) => sum + s.expense_count, 0) ?? 0,
)

// 预算告警只用整数计算：已用 = 额度 - 可用，避免浮点比较。
interface BudgetAlert {
  budget: Budget
  percent: number
  over: boolean
}

const budgetAlerts = computed<BudgetAlert[]>(() =>
  budgets.value
    .map((budget) => {
      const used = budget.amount_minor - budget.available_minor
      return {
        budget,
        percent: budget.amount_minor > 0 ? Math.round((used * 100) / budget.amount_minor) : 0,
        over: budget.available_minor <= 0,
      }
    })
    .filter((a) => a.percent >= 90)
    .sort((a, b) => b.percent - a.percent)
    .slice(0, 6),
)

const chartEl = ref<HTMLDivElement>()
let chart: ECharts | null = null
let echartsLib: typeof import('echarts/core') | null = null

async function renderChart(): Promise<void> {
  if (!chartEl.value) return
  if (!echartsLib) echartsLib = (await import('@/utils/charts')).default
  if (!chart) chart = echartsLib.init(chartEl.value)

  const dash = data.value
  if (!dash || dash.daily_totals.length === 0) {
    chart.clear()
    return
  }

  const currencies = Array.from(new Set(dash.daily_totals.map((d) => d.currency))).sort()
  const dates = Array.from(new Set(dash.daily_totals.map((d) => d.date))).sort()

  const series = currencies.map((currency) => {
    const exp = currencyExponent(currency)
    return {
      name: currency,
      type: 'line' as const,
      smooth: true,
      symbol: 'circle',
      symbolSize: 6,
      connectNulls: true,
      data: dates.map((date) => {
        const row = dash.daily_totals.find((d) => d.date === date && d.currency === currency)
        return row ? row.amount_minor / 10 ** exp : null
      }),
    }
  })

  chart.setOption(
    {
      color: palette,
      tooltip: {
        trigger: 'axis',
        formatter: (params: unknown) => {
          const list = (Array.isArray(params) ? params : [params]) as {
            seriesName: string
            name: string
            value: number | null
          }[]
          if (!list.length) return ''
          const lines = [list[0].name]
          for (const item of list) {
            if (item.value === null) continue
            const row = dash.daily_totals.find(
              (d) => d.date === list[0].name && d.currency === item.seriesName,
            )
            const amount = row ? formatMoney(row.amount_minor, row.currency) : String(item.value)
            lines.push(`${item.seriesName}：${amount}`)
          }
          return lines.join('<br/>')
        },
      },
      legend: { top: 0 },
      grid: { left: 16, right: 16, top: 48, bottom: 16, containLabel: true },
      xAxis: { type: 'category', boundaryGap: false, data: dates, axisTick: { show: false } },
      yAxis: {
        type: 'value',
        name: '金额（主单位）',
        splitLine: { lineStyle: { color: '#eef1f6' } },
      },
      series,
    },
    true,
  )
  chart.resize()
}

async function load(): Promise<void> {
  if (!range.value) return
  loading.value = true
  try {
    const [from, to] = range.value
    const [dash, budgetRes, inboxRes] = await Promise.allSettled([
      reportApi.dashboard({ from, to, scope: auth.isAdmin ? scope.value : 'mine' }),
      budgetApi.list({ page_size: 100 }),
      auth.isAdmin
        ? expenseApi.list({ inbox: true, page: 1, page_size: 1 })
        : Promise.resolve(null),
    ])

    if (dash.status === 'fulfilled') {
      data.value = dash.value
      await nextTick()
      await renderChart()
    } else {
      ElMessage.error(apiMessage(dash.reason))
    }
    budgets.value = budgetRes.status === 'fulfilled' ? budgetRes.value.items : []
    inboxTotal.value =
      inboxRes.status === 'fulfilled' && inboxRes.value ? inboxRes.value.total : 0
  } finally {
    loading.value = false
  }
}

function resize(): void {
  chart?.resize()
}

onMounted(() => {
  void load()
  window.addEventListener('resize', resize)
})

onBeforeUnmount(() => {
  window.removeEventListener('resize', resize)
  chart?.dispose()
  chart = null
})
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2>概览</h2>
        <p>金额按币种分列，从不跨币种相加；预算告警不参与提交时的额度校验。</p>
      </div>
      <div class="toolbar">
        <el-select v-if="auth.isAdmin" v-model="scope" style="width: 120px">
          <el-option value="all" label="全租户" />
          <el-option value="mine" label="仅我的" />
        </el-select>
        <el-date-picker
          v-model="range"
          type="daterange"
          value-format="YYYY-MM-DD"
          range-separator="至"
          start-placeholder="开始日期"
          end-placeholder="结束日期"
          style="width: 260px"
          @change="load"
        />
        <el-button type="primary" :loading="loading" @click="load">刷新</el-button>
      </div>
    </div>

    <div v-loading="loading" class="summary-grid">
      <div v-for="total in data?.currency_totals ?? []" :key="total.currency" class="stat-card">
        <div class="label">{{ total.currency }} · 区间花费</div>
        <div class="value">{{ formatMoney(total.amount_minor, total.currency) }}</div>
        <div class="muted" style="font-size: 12px; margin-top: 4px">
          {{ total.expense_count }} 笔
        </div>
      </div>
      <div v-if="!data?.currency_totals?.length" class="stat-card">
        <div class="label">区间内没有数据</div>
        <div class="value">—</div>
      </div>
      <div class="stat-card">
        <div class="label">单据总数</div>
        <div class="value">{{ totalExpenses }}</div>
      </div>
      <div class="stat-card">
        <div class="label">待审批</div>
        <div class="value">{{ pendingCount }}</div>
        <div v-if="auth.isAdmin" class="muted" style="font-size: 12px; margin-top: 4px">
          待我审批 {{ inboxTotal }} 张
        </div>
      </div>
      <div class="stat-card">
        <div class="label">已批准 / 已报销</div>
        <div class="value">{{ settledCount }}</div>
      </div>
    </div>

    <div class="section-card">
      <div style="display: flex; justify-content: space-between; align-items: center">
        <div>
          <h3 style="margin: 0">预算告警</h3>
          <p class="muted" style="margin: 4px 0 0; font-size: 13px">
            已用（预占 + 支出）达到额度 90% 的预算，最多显示 6 条。
          </p>
        </div>
        <el-button text type="primary" @click="router.push('/budgets')">查看全部预算</el-button>
      </div>

      <el-empty
        v-if="budgetAlerts.length === 0"
        description="没有接近上限的预算"
        :image-size="60"
      />
      <div v-else class="alert-list">
        <div v-for="alert in budgetAlerts" :key="alert.budget.id" class="alert-row">
          <div class="alert-meta">
            <span class="alert-name">{{ alert.budget.name }}</span>
            <el-tag :type="alert.over ? 'danger' : 'warning'" size="small" effect="plain">
              {{ alert.over ? '已超限' : '接近上限' }}
            </el-tag>
            <span class="muted">
              {{ formatMoney(alert.budget.amount_minor - alert.budget.available_minor,
                alert.budget.currency) }}
              / {{ formatMoney(alert.budget.amount_minor, alert.budget.currency) }}
              （{{ alert.percent }}%）
            </span>
          </div>
          <el-progress
            :percentage="percentOf(alert.budget.amount_minor - alert.budget.available_minor,
              alert.budget.amount_minor)"
            :stroke-width="8"
            :status="alert.over ? 'exception' : 'warning'"
            style="flex: 1; min-width: 120px"
          />
        </div>
      </div>
    </div>

    <div class="section-card">
      <h3>每日费用</h3>
      <div v-show="data && data.daily_totals.length > 0" ref="chartEl" class="chart" />
      <el-empty v-if="data && data.daily_totals.length === 0" description="所选区间没有费用" />
    </div>

    <div class="section-card">
      <h3>费用类别</h3>
      <el-table :data="data?.category_totals ?? []" stripe>
        <el-table-column label="类别" min-width="160">
          <template #default="{ row }">{{ categoryLabel(row.group) }}</template>
        </el-table-column>
        <el-table-column label="币种" width="100" prop="currency" />
        <el-table-column label="笔数" width="100" prop="expense_count" align="right" />
        <el-table-column label="金额" width="180" align="right">
          <template #default="{ row }">
            <span class="money">{{ formatMoney(row.amount_minor, row.currency) }}</span>
          </template>
        </el-table-column>
        <template #empty>
          <el-empty description="所选区间没有费用" />
        </template>
      </el-table>
    </div>

    <div class="section-card">
      <h3>状态分布</h3>
      <div class="status-list">
        <div v-for="row in data?.status_totals ?? []" :key="row.status + row.currency" class="status-row">
          <el-tag size="small" effect="plain">{{ statusLabel(row.status) }}</el-tag>
          <span class="muted">{{ row.expense_count }} 笔</span>
          <span class="money">{{ formatMoney(row.amount_minor, row.currency) }}</span>
        </div>
        <el-empty
          v-if="!data?.status_totals?.length"
          description="所选区间没有费用"
          :image-size="60"
        />
      </div>
    </div>
  </div>
</template>

<style scoped>
.alert-list {
  display: flex;
  flex-direction: column;
  gap: 12px;
  margin-top: 14px;
}

.alert-row {
  display: flex;
  align-items: center;
  gap: 16px;
  flex-wrap: wrap;
}

.alert-meta {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 260px;
  flex-wrap: wrap;
}

.alert-name {
  font-weight: 600;
  font-size: 13px;
}

.status-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-top: 12px;
}

.status-row {
  display: flex;
  align-items: center;
  gap: 12px;
}
</style>
