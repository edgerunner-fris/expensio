<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { apiMessage } from '@/api/client'
import { approvalApi, expenseApi } from '@/api/expenses'
import type { ApprovalPolicy, Expense } from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import {
  categoryLabel,
  currencies,
  formatDate,
  formatMoney,
  minorToInput,
  parseMoneyToMinor,
} from '@/utils/format'

const router = useRouter()
const auth = useAuthStore()

// ---- 待办队列 ----
const items = ref<Expense[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = 10
const loading = ref(false)

async function loadInbox(): Promise<void> {
  loading.value = true
  try {
    const result = await expenseApi.list({ inbox: true, page: page.value, page_size: pageSize })
    items.value = result.items
    total.value = result.total
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    loading.value = false
  }
}

function openExpense(row: Expense): void {
  void router.push(`/expenses/${row.id}`)
}

onMounted(() => {
  void loadInbox()
  if (auth.isAdmin) void loadPolicies()
})

// ---- 审批策略 ----
const policies = ref<ApprovalPolicy[]>([])
const policyVisible = ref(false)
const policySaving = ref(false)
const policyForm = reactive({ currency: 'USD', threshold: '' })

async function loadPolicies(): Promise<void> {
  try {
    policies.value = (await approvalApi.listPolicies()).items
  } catch (err) {
    ElMessage.error(apiMessage(err))
  }
}

function openPolicy(policy?: ApprovalPolicy): void {
  if (policy) {
    policyForm.currency = policy.currency
    policyForm.threshold = minorToInput(policy.second_level_threshold_minor, policy.currency)
  } else {
    policyForm.currency = currencies[0]
    policyForm.threshold = ''
  }
  policyVisible.value = true
}

async function savePolicy(): Promise<void> {
  const currency = policyForm.currency.toUpperCase()
  const minor = parseMoneyToMinor(policyForm.threshold, currency)
  if (minor === null || minor <= 0) {
    ElMessage.error('请输入大于 0 的金额')
    return
  }
  policySaving.value = true
  try {
    await approvalApi.upsertPolicy(currency, minor)
    ElMessage.success('审批策略已保存，只影响之后提交的单据')
    policyVisible.value = false
    await loadPolicies()
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    policySaving.value = false
  }
}

async function removePolicy(policy: ApprovalPolicy): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `删除 ${policy.currency} 的二级审批门槛后，该币种之后提交的单据只需一级审批。`,
      '删除审批策略',
      { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await approvalApi.removePolicy(policy.currency)
    ElMessage.success('已删除')
    await loadPolicies()
  } catch (err) {
    ElMessage.error(apiMessage(err))
  }
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2>待我审批</h2>
        <p>只显示已提交、且你本人还没有批过的单据。多级审批要求不同的人各批一级。</p>
      </div>
    </div>

    <div class="section-card">
      <el-table v-loading="loading" :data="items" empty-text="没有待审批的单据" @row-click="openExpense">
        <el-table-column label="金额" width="150">
          <template #default="{ row }">
            <span class="money">{{ formatMoney(row.amount_minor, row.currency) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="类别" width="100">
          <template #default="{ row }">{{ categoryLabel(row.category) }}</template>
        </el-table-column>
        <el-table-column prop="merchant" label="商户" min-width="160" show-overflow-tooltip />
        <el-table-column label="发生日期" width="120">
          <template #default="{ row }">{{ formatDate(row.occurred_at) }}</template>
        </el-table-column>
        <el-table-column label="审批进度" width="130">
          <template #default="{ row }">
            <el-tag v-if="row.required_approval_levels > 1" type="warning" effect="plain">
              {{ row.approval_level }} / {{ row.required_approval_levels }} 级
            </el-tag>
            <span v-else class="muted">单级</span>
          </template>
        </el-table-column>
        <el-table-column label="" width="110" align="right">
          <template #default="{ row }">
            <el-button type="primary" text @click.stop="openExpense(row)">去审批</el-button>
          </template>
        </el-table-column>
      </el-table>

      <div style="display: flex; justify-content: flex-end; margin-top: 12px">
        <el-pagination
          v-model:current-page="page"
          :page-size="pageSize"
          :total="total"
          layout="total, prev, pager, next"
          @current-change="loadInbox"
        />
      </div>
    </div>

    <div class="section-card" style="margin-top: 18px">
      <div style="display: flex; justify-content: space-between; align-items: center">
        <div>
          <h3 style="margin: 0">分级审批策略</h3>
          <p class="muted" style="margin: 4px 0 0; font-size: 13px">
            按币种设置金额门槛：达到门槛的单据需要两级审批，由两位不同的管理员分别批准。
            策略只对之后提交的单据生效。
          </p>
        </div>
        <el-button type="primary" @click="openPolicy()">新增策略</el-button>
      </div>

      <el-table :data="policies" empty-text="没有策略：所有单据都是单级审批" style="margin-top: 16px">
        <el-table-column prop="currency" label="币种" width="100" />
        <el-table-column label="二级审批门槛" min-width="180">
          <template #default="{ row }">
            {{ formatMoney(row.second_level_threshold_minor, row.currency) }} 及以上
          </template>
        </el-table-column>
        <el-table-column label="更新时间" width="170">
          <template #default="{ row }">{{ formatDate(row.updated_at) }}</template>
        </el-table-column>
        <el-table-column label="" width="160" align="right">
          <template #default="{ row }">
            <el-button text @click="openPolicy(row)">修改</el-button>
            <el-button text type="danger" @click="removePolicy(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>
    </div>

    <el-dialog v-model="policyVisible" title="二级审批门槛" width="420px" destroy-on-close>
      <el-form label-position="top">
        <el-form-item label="币种">
          <el-select v-model="policyForm.currency" style="width: 100%">
            <el-option v-for="c in currencies" :key="c" :value="c" :label="c" />
          </el-select>
        </el-form-item>
        <el-form-item label="金额达到多少需要两级审批">
          <el-input v-model="policyForm.threshold" placeholder="例如 5000.00" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="policyVisible = false">取消</el-button>
        <el-button type="primary" :loading="policySaving" @click="savePolicy">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>
