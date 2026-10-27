<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { apiMessage, errorCode } from '@/api/client'
import { billingApi } from '@/api/billing'
import type { Invoice, Plan, Subscription } from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import { formatDate, formatDateTime, formatMoney } from '@/utils/format'

const auth = useAuthStore()

const plans = ref<Plan[]>([])
const subscription = ref<Subscription | null>(null)
const invoices = ref<Invoice[]>([])
const invoiceTotal = ref(0)
const invoicePage = ref(1)
const invoicePageSize = 10
const invoiceStatus = ref<'' | 'open' | 'paid'>('')
const loading = ref(false)
const busy = ref(false)

const selectedPlan = ref('')
const seats = ref(5)

const planByCode = computed(() => {
  const map = new Map<string, Plan>()
  for (const plan of plans.value) map.set(plan.code, plan)
  return map
})

const selected = computed(() => planByCode.value.get(selectedPlan.value) ?? null)
const selectedTotal = computed(() =>
  selected.value ? selected.value.price_minor * seats.value : 0,
)

const invoiceDialog = ref(false)
const detail = ref<Invoice | null>(null)

async function loadPlans(): Promise<void> {
  plans.value = await billingApi.plans()
  if (!selectedPlan.value && plans.value.length > 0) {
    selectedPlan.value = plans.value[0].code
  }
}

async function loadSubscription(): Promise<void> {
  try {
    subscription.value = await billingApi.currentSubscription()
  } catch (err) {
    if (errorCode(err) === 'not_found') {
      subscription.value = null
      return
    }
    throw err
  }
}

async function loadInvoices(): Promise<void> {
  const result = await billingApi.listInvoices(
    invoiceStatus.value,
    invoicePage.value,
    invoicePageSize,
  )
  invoices.value = result.items
  invoiceTotal.value = result.total
}

async function loadAll(): Promise<void> {
  loading.value = true
  try {
    await Promise.all([loadPlans(), loadSubscription(), loadInvoices()])
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    loading.value = false
  }
}

async function onInvoicePage(next: number): Promise<void> {
  invoicePage.value = next
  try {
    await loadInvoices()
  } catch (err) {
    ElMessage.error(apiMessage(err))
  }
}

async function onInvoiceStatusChange(): Promise<void> {
  invoicePage.value = 1
  try {
    await loadInvoices()
  } catch (err) {
    ElMessage.error(apiMessage(err))
  }
}

async function subscribe(): Promise<void> {
  if (!selected.value) return
  busy.value = true
  try {
    await billingApi.subscribe(selected.value.code, seats.value)
    ElMessage.success('订阅已开通')
    await Promise.all([loadSubscription(), loadInvoices()])
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    busy.value = false
  }
}

async function cancel(): Promise<void> {
  const sub = subscription.value
  if (!sub) return
  try {
    await ElMessageBox.confirm('取消后不能再为当前订阅出账，已开的账单仍然需要支付。', '取消订阅', {
      confirmButtonText: '确认取消',
      cancelButtonText: '返回',
      type: 'warning',
    })
    busy.value = true
    subscription.value = await billingApi.cancel(sub.id, sub.version)
    ElMessage.success('订阅已取消')
  } catch (err) {
    if (err !== 'cancel' && err !== 'close') ElMessage.error(apiMessage(err))
  } finally {
    busy.value = false
  }
}

async function generateInvoice(): Promise<void> {
  const sub = subscription.value
  if (!sub) return
  busy.value = true
  try {
    const invoice = await billingApi.generateInvoice(sub.id)
    ElMessage.success(`账单 ${invoice.number} 已生成`)
    await loadInvoices()
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    busy.value = false
  }
}

async function openInvoice(id: string): Promise<void> {
  try {
    detail.value = await billingApi.getInvoice(id)
    invoiceDialog.value = true
  } catch (err) {
    ElMessage.error(apiMessage(err))
  }
}

function intervalLabel(interval: string): string {
  return interval === 'yearly' ? '每年' : '每月'
}

onMounted(loadAll)
</script>

<template>
  <div v-loading="loading" class="page">
    <div class="page-head">
      <div>
        <h2>账单与订阅</h2>
        <p>订阅按席位计费；账单金额 = 套餐单价 × 席位数，全部以整数最小单位结算。</p>
      </div>
    </div>

    <el-alert
      v-if="subscription?.status === 'past_due'"
      type="error"
      :closable="false"
      show-icon
      title="最近一次付款失败，订阅已进入逾期状态。请尽快完成支付。"
    />

    <div class="section-card">
      <h3>当前订阅</h3>
      <template v-if="subscription">
        <div class="sub-grid">
          <div>
            <div class="muted">套餐</div>
            <div class="sub-value">
              {{ planByCode.get(subscription.plan_code)?.name ?? subscription.plan_code }}
            </div>
          </div>
          <div>
            <div class="muted">状态</div>
            <div class="sub-value">
              <el-tag :type="subscription.status === 'active' ? 'success' : subscription.status === 'past_due' ? 'danger' : 'info'">
                {{ subscription.status === 'active' ? '生效中' : subscription.status === 'past_due' ? '逾期' : '已取消' }}
              </el-tag>
            </div>
          </div>
          <div>
            <div class="muted">席位</div>
            <div class="sub-value">{{ subscription.seats }}</div>
          </div>
          <div>
            <div class="muted">当前周期</div>
            <div class="sub-value">
              {{ formatDate(subscription.current_period_start) }} ~
              {{ formatDate(subscription.current_period_end) }}
            </div>
          </div>
          <div>
            <div class="muted">版本</div>
            <div class="sub-value">{{ subscription.version }}</div>
          </div>
        </div>
        <div v-if="auth.isAdmin" class="toolbar" style="margin-top: 16px">
          <el-button
            type="primary"
            :disabled="subscription.status === 'canceled'"
            :loading="busy"
            @click="generateInvoice"
          >
            生成本期账单
          </el-button>
          <el-button
            type="danger"
            plain
            :disabled="subscription.status === 'canceled'"
            :loading="busy"
            @click="cancel"
          >
            取消订阅
          </el-button>
        </div>
      </template>
      <el-empty v-else description="还没有订阅，选择下方套餐即可开通" />
    </div>

    <div v-if="!subscription && auth.isAdmin" class="section-card">
      <h3>选择套餐</h3>
      <div class="plan-grid">
        <div
          v-for="plan in plans"
          :key="plan.code"
          class="plan-card"
          :class="{ selected: selectedPlan === plan.code }"
          @click="selectedPlan = plan.code"
        >
          <div class="plan-name">{{ plan.name }}</div>
          <div class="plan-price">
            {{ formatMoney(plan.price_minor, plan.currency) }}
            <span class="muted">/ {{ intervalLabel(plan.interval) }}</span>
          </div>
          <div class="muted" style="font-size: 12px">按席位计费 · {{ plan.code }}</div>
        </div>
      </div>

      <div class="toolbar" style="margin-top: 18px">
        <span>席位数</span>
        <el-input-number v-model="seats" :min="1" :max="1000" />
        <span class="muted">
          合计 {{ selected ? formatMoney(selectedTotal, selected.currency) : '—' }} /
          {{ selected ? intervalLabel(selected.interval) : '—' }}
        </span>
        <el-button type="primary" :disabled="!selected" :loading="busy" @click="subscribe">
          开通订阅
        </el-button>
      </div>
    </div>

    <div class="section-card">
      <div class="toolbar" style="margin-bottom: 14px">
        <h3 style="margin: 0">账单</h3>
        <span class="spacer" />
        <el-select v-model="invoiceStatus" style="width: 130px" @change="onInvoiceStatusChange">
          <el-option value="" label="全部状态" />
          <el-option value="open" label="待支付" />
          <el-option value="paid" label="已支付" />
        </el-select>
      </div>

      <el-table :data="invoices" stripe>
        <el-table-column label="账单号" min-width="180">
          <template #default="{ row }">
            <el-button link type="primary" @click="openInvoice(row.id)">{{ row.number }}</el-button>
          </template>
        </el-table-column>
        <el-table-column label="账期" width="210">
          <template #default="{ row }">
            {{ formatDate(row.period_start) }} ~ {{ formatDate(row.period_end) }}
          </template>
        </el-table-column>
        <el-table-column label="金额" width="150" align="right">
          <template #default="{ row }">
            <span class="money">{{ formatMoney(row.amount_minor, row.currency) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="到期日" width="120">
          <template #default="{ row }">{{ formatDate(row.due_on) }}</template>
        </el-table-column>
        <el-table-column label="状态" width="100">
          <template #default="{ row }">
            <el-tag :type="row.status === 'paid' ? 'success' : 'warning'" size="small">
              {{ row.status === 'paid' ? '已支付' : '待支付' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="支付时间" width="160">
          <template #default="{ row }">{{ formatDateTime(row.paid_at) }}</template>
        </el-table-column>
        <template #empty>
          <el-empty description="还没有账单" />
        </template>
      </el-table>

      <div style="display: flex; justify-content: flex-end; margin-top: 14px">
        <el-pagination
          layout="total, prev, pager, next"
          :total="invoiceTotal"
          :page-size="invoicePageSize"
          :current-page="invoicePage"
          @current-change="onInvoicePage"
        />
      </div>
    </div>

    <el-dialog v-model="invoiceDialog" title="账单详情" width="480px">
      <el-descriptions v-if="detail" :column="1" border>
        <el-descriptions-item label="账单号">{{ detail.number }}</el-descriptions-item>
        <el-descriptions-item label="金额">
          {{ formatMoney(detail.amount_minor, detail.currency) }}
        </el-descriptions-item>
        <el-descriptions-item label="账期">
          {{ formatDate(detail.period_start) }} ~ {{ formatDate(detail.period_end) }}
        </el-descriptions-item>
        <el-descriptions-item label="到期日">{{ formatDate(detail.due_on) }}</el-descriptions-item>
        <el-descriptions-item label="状态">
          <el-tag :type="detail.status === 'paid' ? 'success' : 'warning'" size="small">
            {{ detail.status === 'paid' ? '已支付' : '待支付' }}
          </el-tag>
        </el-descriptions-item>
        <el-descriptions-item label="签发时间">{{ formatDateTime(detail.issued_at) }}</el-descriptions-item>
        <el-descriptions-item label="支付时间">{{ formatDateTime(detail.paid_at) }}</el-descriptions-item>
      </el-descriptions>
    </el-dialog>
  </div>
</template>

<style scoped>
.sub-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
  gap: 16px;
}

.sub-value {
  margin-top: 4px;
  font-weight: 600;
}

.plan-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 12px;
}

.plan-card {
  border: 1px solid var(--expensio-border);
  border-radius: 12px;
  padding: 16px;
  cursor: pointer;
  transition: border-color 0.15s, box-shadow 0.15s;
}

.plan-card:hover {
  border-color: #b9cdf8;
}

.plan-card.selected {
  border-color: var(--expensio-primary);
  box-shadow: 0 0 0 2px #dbe7ff inset;
}

.plan-name {
  font-weight: 650;
}

.plan-price {
  margin: 8px 0 6px;
  font-size: 20px;
  font-weight: 700;
}
</style>
