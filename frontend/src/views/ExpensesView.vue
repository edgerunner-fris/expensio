<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { Plus, Search } from '@element-plus/icons-vue'
import { apiMessage } from '@/api/client'
import { budgetApi, expenseApi } from '@/api/expenses'
import type { Budget, Category, Expense } from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import {
  categoryLabel,
  categoryOptions,
  currencies,
  daysAgoISO,
  formatDate,
  formatMoney,
  occurredAtISO,
  parseMoneyToMinor,
  statusLabel,
  statusTagType,
} from '@/utils/format'

const router = useRouter()
const auth = useAuthStore()

const items = ref<Expense[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = 20
const loading = ref(false)

const filters = reactive<{ scope: 'mine' | 'all'; status: string }>({
  scope: auth.isAdmin ? 'all' : 'mine',
  status: '',
})

const statusOptions = [
  { value: 'draft', label: '草稿' },
  { value: 'submitted', label: '待审批' },
  { value: 'approved', label: '已批准' },
  { value: 'rejected', label: '已驳回' },
  { value: 'reimbursed', label: '已报销' },
  { value: 'cancelled', label: '已撤销' },
]

async function load(): Promise<void> {
  loading.value = true
  try {
    const result = await expenseApi.list({
      scope: filters.scope,
      status: filters.status || undefined,
      page: page.value,
      page_size: pageSize,
    })
    items.value = result.items
    total.value = result.total
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    loading.value = false
  }
}

watch(
  () => [filters.scope, filters.status],
  () => {
    page.value = 1
    void load()
  },
)

function onPageChange(next: number): void {
  page.value = next
  void load()
}

onMounted(load)

// ---- 新建草稿 ----
const dialogVisible = ref(false)
const submitting = ref(false)
const formRef = ref<FormInstance>()
const budgets = ref<Budget[]>([])

const form = reactive({
  category: 'travel' as Category,
  currency: 'USD',
  amount: '',
  merchant: '',
  description: '',
  occurred_at: daysAgoISO(0),
  budget_id: '',
})

const matchingBudgets = computed(() =>
  budgets.value.filter((b) => b.category === form.category && b.currency === form.currency),
)

const rules: FormRules = {
  category: [{ required: true, message: '请选择类别', trigger: 'change' }],
  currency: [{ required: true, message: '请选择币种', trigger: 'change' }],
  amount: [
    { required: true, message: '请输入金额', trigger: 'blur' },
    {
      validator: (_rule, value, callback) => {
        if (parseMoneyToMinor(String(value), form.currency) === null) {
          callback(new Error('金额格式不正确，最多两位小数'))
          return
        }
        callback()
      },
      trigger: 'blur',
    },
  ],
  merchant: [
    { required: true, message: '请输入商户名称', trigger: 'blur' },
    { max: 200, message: '商户名称不超过 200 个字符', trigger: 'blur' },
  ],
  description: [{ max: 2000, message: '说明不超过 2000 个字符', trigger: 'blur' }],
  occurred_at: [{ required: true, message: '请选择发生日期', trigger: 'change' }],
}

function resetForm(): void {
  form.category = 'travel'
  form.currency = 'USD'
  form.amount = ''
  form.merchant = ''
  form.description = ''
  form.occurred_at = daysAgoISO(0)
  form.budget_id = ''
  formRef.value?.clearValidate()
}

async function openCreate(): Promise<void> {
  resetForm()
  dialogVisible.value = true
  try {
    const result = await budgetApi.list({ page: 1, page_size: 100 })
    budgets.value = result.items
  } catch {
    // 预算列表拿不到不影响建单，最多是预算下拉为空。
    budgets.value = []
  }
}

watch(
  () => [form.category, form.currency],
  () => {
    if (budgets.value.length > 0 && form.budget_id && !matchingBudgets.value.some((b) => b.id === form.budget_id)) {
      form.budget_id = ''
    }
  },
)

async function submit(): Promise<void> {
  if (!formRef.value) return
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) return
  const amountMinor = parseMoneyToMinor(form.amount, form.currency)
  if (amountMinor === null) return

  submitting.value = true
  try {
    const occurredAt = occurredAtISO(form.occurred_at)
    const created = await expenseApi.create({
      category: form.category,
      currency: form.currency,
      amount_minor: amountMinor,
      merchant: form.merchant.trim(),
      description: form.description.trim(),
      occurred_at: occurredAt,
      ...(form.budget_id ? { budget_id: form.budget_id } : {}),
    })
    ElMessage.success('草稿已创建')
    dialogVisible.value = false
    await router.push(`/expenses/${created.id}`)
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2>费用单</h2>
        <p>提交、审批与报销都在这里流转；金额以整数最小单位存储。</p>
      </div>
      <el-button v-if="auth.canWrite" type="primary" :icon="Plus" @click="openCreate">
        新建费用单
      </el-button>
    </div>

    <div class="section-card">
      <div class="toolbar" style="margin-bottom: 14px">
        <el-select v-model="filters.scope" style="width: 140px">
          <el-option value="mine" label="我的费用单" />
          <el-option v-if="auth.isAdmin" value="all" label="全租户" />
        </el-select>
        <el-select v-model="filters.status" style="width: 140px" placeholder="全部状态">
          <el-option value="" label="全部状态" />
          <el-option
            v-for="opt in statusOptions"
            :key="opt.value"
            :value="opt.value"
            :label="opt.label"
          />
        </el-select>
        <el-button :icon="Search" @click="load">刷新</el-button>
        <span class="spacer" />
        <span class="muted">共 {{ total }} 条</span>
      </div>

      <el-table v-loading="loading" :data="items" stripe>
        <el-table-column label="发生日期" width="120">
          <template #default="{ row }">{{ formatDate(row.occurred_at) }}</template>
        </el-table-column>
        <el-table-column label="类别" width="100">
          <template #default="{ row }">{{ categoryLabel(row.category) }}</template>
        </el-table-column>
        <el-table-column label="商户 / 说明" min-width="240">
          <template #default="{ row }">
            <div>{{ row.merchant }}</div>
            <div v-if="row.description" class="muted" style="font-size: 12px">
              {{ row.description }}
            </div>
          </template>
        </el-table-column>
        <el-table-column label="金额" width="140" align="right">
          <template #default="{ row }">
            <span class="money">{{ formatMoney(row.amount_minor, row.currency) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="110">
          <template #default="{ row }">
            <el-tag :type="statusTagType(row.status)" size="small">
              {{ statusLabel(row.status) }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="90" align="right">
          <template #default="{ row }">
            <el-button link type="primary" @click="router.push(`/expenses/${row.id}`)">
              查看
            </el-button>
          </template>
        </el-table-column>
        <template #empty>
          <el-empty description="还没有费用单" />
        </template>
      </el-table>

      <div style="display: flex; justify-content: flex-end; margin-top: 14px">
        <el-pagination
          layout="total, prev, pager, next"
          :total="total"
          :page-size="pageSize"
          :current-page="page"
          @current-change="onPageChange"
        />
      </div>
    </div>

    <el-dialog v-model="dialogVisible" title="新建费用单" width="520px" destroy-on-close>
      <el-form ref="formRef" :model="form" :rules="rules" label-position="top">
        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="类别" prop="category">
              <el-select v-model="form.category" style="width: 100%">
                <el-option
                  v-for="opt in categoryOptions"
                  :key="opt.value"
                  :value="opt.value"
                  :label="opt.label"
                />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="发生日期" prop="occurred_at">
              <el-date-picker
                v-model="form.occurred_at"
                type="date"
                value-format="YYYY-MM-DD"
                style="width: 100%"
              />
            </el-form-item>
          </el-col>
        </el-row>

        <el-form-item label="金额" prop="amount">
          <el-input v-model="form.amount" placeholder="例如 128.50">
            <template #append>
              <el-select v-model="form.currency" style="width: 90px">
                <el-option v-for="c in currencies" :key="c" :value="c" :label="c" />
              </el-select>
            </template>
          </el-input>
        </el-form-item>

        <el-form-item label="商户" prop="merchant">
          <el-input v-model="form.merchant" maxlength="200" placeholder="例如 汉莎航空" />
        </el-form-item>

        <el-form-item label="说明" prop="description">
          <el-input
            v-model="form.description"
            type="textarea"
            :rows="3"
            maxlength="2000"
            show-word-limit
            placeholder="可选：同行人、项目、发票编号"
          />
        </el-form-item>

        <el-form-item label="关联预算（可选）">
          <el-select v-model="form.budget_id" clearable style="width: 100%" placeholder="不关联预算">
            <el-option
              v-for="b in matchingBudgets"
              :key="b.id"
              :value="b.id"
              :label="`${b.name}（可用 ${formatMoney(b.available_minor, b.currency)}）`"
            />
          </el-select>
          <div v-if="!matchingBudgets.length" class="muted" style="font-size: 12px">
            当前类别与币种下没有可用预算
          </div>
        </el-form-item>
      </el-form>

      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="submit">保存草稿</el-button>
      </template>
    </el-dialog>
  </div>
</template>
