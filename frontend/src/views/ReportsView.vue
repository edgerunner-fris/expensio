<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Download } from '@element-plus/icons-vue'
import type { ECharts } from 'echarts/core'
import { apiMessage } from '@/api/client'
import { reportApi } from '@/api/reports'
import type { ExpenseSummary } from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import {
  categoryLabel,
  currencyExponent,
  daysAgoISO,
  formatMoney,
  statusLabel,
} from '@/utils/format'

const auth = useAuthStore()

const range = ref<[string, string] | null>([daysAgoISO(29), daysAgoISO(0)])
const filters = reactive<{ scope: 'mine' | 'all'; group_by: 'category' | 'status' }>({
  scope: auth.isAdmin ? 'all' : 'mine',
  group_by: 'category',
})
const summary = ref<ExpenseSummary | null>(null)
const loading = ref(false)
const exporting = ref(false)

async function exportCSV(): Promise<void> {
  if (!range.value || exporting.value) return
  exporting.value = true
  const [from, to] = range.value
  try {
    const blob = await reportApi.exportCSV({
      from,
      to,
      group_by: filters.group_by,
      scope: auth.isAdmin ? filters.scope : 'mine',
    })
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = `expenses-${from}-${to}.csv`
    document.body.appendChild(link)
    link.click()
    link.remove()
    URL.revokeObjectURL(url)
    ElMessage.success('明细已导出')
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    exporting.value = false
  }
}

const chartEl = ref<HTMLDivElement>()
let chart: ECharts | null = null
let echartsLib: typeof import('echarts/core') | null = null

const palette = ['#2f6feb', '#22a06b', '#d97706', '#8b5cf6', '#e11d48', '#0ea5e9']

function groupLabel(group: string): string {
  return filters.group_by === 'category' ? categoryLabel(group) : statusLabel(group)
}

async function load(): Promise<void> {
  if (!range.value) return
  loading.value = true
  try {
    summary.value = await reportApi.summary({
      from: range.value[0],
      to: range.value[1],
      group_by: filters.group_by,
      scope: auth.isAdmin ? filters.scope : 'mine',
    })
    await nextTick()
    await renderChart()
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    loading.value = false
  }
}

async function renderChart(): Promise<void> {
  if (!chartEl.value) return
  if (!echartsLib) echartsLib = (await import('@/utils/charts')).default
  if (!chart) chart = echartsLib.init(chartEl.value)

  const data = summary.value
  if (!data || data.groups.length === 0) {
    chart.clear()
    return
  }

  // 每个币种一条独立 series：不同币种之间绝不堆叠求和。
  // 图表里的数值仅用于展示（除以该币种的小数位），业务金额始终是整数最小单位。
  const currencies = Array.from(new Set(data.groups.map((g) => g.currency))).sort()
  const groups = Array.from(new Set(data.groups.map((g) => g.group)))

  const series = currencies.map((currency) => {
    const exp = currencyExponent(currency)
    return {
      name: currency,
      type: 'bar' as const,
      barMaxWidth: 42,
      data: groups.map((group) => {
        const row = data.groups.find((g) => g.group === group && g.currency === currency)
        return row ? row.amount_minor / 10 ** exp : null
      }),
    }
  })

  chart.setOption(
    {
      color: palette,
      tooltip: {
        trigger: 'axis',
        axisPointer: { type: 'shadow' },
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
            const row = data.groups.find(
              (g) => g.group === list[0].name && g.currency === item.seriesName,
            )
            const amount = row ? formatMoney(row.amount_minor, row.currency) : String(item.value)
            lines.push(`${item.seriesName}：${amount}`)
          }
          return lines.join('<br/>')
        },
      },
      legend: { top: 0 },
      grid: { left: 16, right: 16, top: 48, bottom: 16, containLabel: true },
      xAxis: {
        type: 'category',
        data: groups.map(groupLabel),
        axisTick: { show: false },
      },
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
        <h2>报表</h2>
        <p>按类别或状态汇总。不同币种分列展示，永远不会相加。</p>
      </div>
      <div class="toolbar">
        <el-select v-if="auth.isAdmin" v-model="filters.scope" style="width: 130px">
          <el-option value="all" label="全租户" />
          <el-option value="mine" label="仅我的" />
        </el-select>
        <el-select v-model="filters.group_by" style="width: 130px">
          <el-option value="category" label="按类别" />
          <el-option value="status" label="按状态" />
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
        <el-button type="primary" :loading="loading" @click="load">查询</el-button>
        <el-button :icon="Download" :loading="exporting" @click="exportCSV">导出 CSV</el-button>
      </div>
    </div>

    <div v-loading="loading" class="summary-grid">
      <div v-for="total in summary?.currency_totals ?? []" :key="total.currency" class="stat-card">
        <div class="label">{{ total.currency }} · {{ total.expense_count }} 笔</div>
        <div class="value">{{ formatMoney(total.amount_minor, total.currency) }}</div>
      </div>
      <div v-if="!summary?.currency_totals?.length" class="stat-card">
        <div class="label">区间内没有数据</div>
        <div class="value">—</div>
      </div>
    </div>

    <div class="section-card">
      <h3>金额分布</h3>
      <div v-show="summary && summary.groups.length > 0" ref="chartEl" class="chart" />
      <el-empty v-if="summary && summary.groups.length === 0" description="所选区间没有可汇总的费用" />
    </div>

    <div class="section-card">
      <h3>明细汇总</h3>
      <el-table :data="summary?.groups ?? []" stripe>
        <el-table-column label="分组" min-width="160">
          <template #default="{ row }">{{ groupLabel(row.group) }}</template>
        </el-table-column>
        <el-table-column label="币种" width="100" prop="currency" />
        <el-table-column label="笔数" width="100" prop="expense_count" align="right" />
        <el-table-column label="金额" width="180" align="right">
          <template #default="{ row }">
            <span class="money">{{ formatMoney(row.amount_minor, row.currency) }}</span>
          </template>
        </el-table-column>
        <template #empty>
          <el-empty description="暂无汇总数据" />
        </template>
      </el-table>
    </div>
  </div>
</template>
