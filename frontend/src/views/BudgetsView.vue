<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { apiMessage } from '@/api/client'
import { budgetApi } from '@/api/expenses'
import type { Budget, Category } from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import {
  categoryLabel,
  categoryOptions,
  currencies,
  formatDate,
  formatMoney,
  isoDate,
  minorToInput,
  parseMoneyToMinor,
  percentOf,
} from '@/utils/format'

const auth = useAuthStore()

const items = ref<Budget[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = 20
const loading = ref(false)
const categoryFilter = ref('')

async function load(): Promise<void> {
  loading.value = true
  try {
    const result = await budgetApi.list({
      category: categoryFilter.value || undefined,
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

watch(categoryFilter, () => {
  page.value = 1
  void load()
})

onMounted(load)

function onPageChange(next: number): void {
  page.value = next
  void load()
}

function monthRange(): { start: string; end: string } {
  const now = new Date()
  const start = new Date(now.getFullYear(), now.getMonth(), 1)
  const end = new Date(now.getFullYear(), now.getMonth() + 1, 0)
  return { start: isoDate(start), end: isoDate(end) }
}

// ---- 新建 / 编辑 ----
const dialogVisible = ref(false)
const editing = ref<Budget | null>(null)
const saving = ref(false)
const formRef = ref<FormInstance>()

const form = reactive({
  name: '',
  category: 'travel' as Category,
  currency: 'USD',
  period_start: '',
  period_end: '',
  amount: '',
})

const rules: FormRules = {
  name: [
    { required: true, message: '请输入预算名称', trigger: 'blur' },
    { max: 120, message: '名称不超过 120 个字符', trigger: 'blur' },
  ],
  category: [{ required: true, message: '请选择类别', trigger: 'change' }],
  currency: [{ required: true, message: '请选择币种', trigger: 'change' }],
  period_start: [{ required: true, message: '请选择开始日期', trigger: 'change' }],
  period_end: [
    { required: true, message: '请选择结束日期', trigger: 'change' },
    {
      validator: (_rule, value, callback) => {
        if (form.period_start && String(value) < form.period_start) {
          callback(new Error('结束日期不能早于开始日期'))
          return
        }
        callback()
      },
      trigger: 'change',
    },
  ],
  amount: [
    { required: true, message: '请输入预算金额', trigger: 'blur' },
    {
      validator: (_rule, value, callback) => {
        const minor = parseMoneyToMinor(String(value), form.currency)
        if (minor === null || minor <= 0) {
          callback(new Error('金额必须是大于 0 的数字'))
          return
        }
        callback()
      },
      trigger: 'blur',
    },
  ],
}

function openCreate(): void {
  editing.value = null
  const range = monthRange()
  form.name = ''
  form.category = 'travel'
  form.currency = 'USD'
  form.period_start = range.start
  form.period_end = range.end
  form.amount = ''
  formRef.value?.clearValidate()
  dialogVisible.value = true
}

function openEdit(budget: Budget): void {
  editing.value = budget
  form.name = budget.name
  form.category = budget.category
  form.currency = budget.currency
  form.period_start = budget.period_start
  form.period_end = budget.period_end
  form.amount = minorToInput(budget.amount_minor, budget.currency)
  formRef.value?.clearValidate()
  dialogVisible.value = true
}

async function save(): Promise<void> {
  if (!formRef.value) return
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) return
  const amountMinor = parseMoneyToMinor(form.amount, form.currency)
  if (amountMinor === null) return

  saving.value = true
  try {
    if (editing.value) {
      await budgetApi.update(editing.value.id, {
        version: editing.value.version,
        name: form.name.trim(),
        period_start: form.period_start,
        period_end: form.period_end,
        amount_minor: amountMinor,
      })
      ElMessage.success('预算已更新')
    } else {
      await budgetApi.create({
        name: form.name.trim(),
        category: form.category,
        currency: form.currency,
        period_start: form.period_start,
        period_end: form.period_end,
        amount_minor: amountMinor,
      })
      ElMessage.success('预算已创建')
    }
    dialogVisible.value = false
    await load()
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    saving.value = false
  }
}

async function remove(budget: Budget): Promise<void> {
  try {
    await ElMessageBox.confirm(`删除预算「${budget.name}」？`, '删除预算', {
      confirmButtonText: '确认删除',
      cancelButtonText: '取消',
      type: 'warning',
    })
    await budgetApi.remove(budget.id, budget.version)
    ElMessage.success('预算已删除')
    await load()
  } catch (err) {
    if (err !== 'cancel' && err !== 'close') ElMessage.error(apiMessage(err))
  }
}

function usedPercent(budget: Budget): number {
  return percentOf(budget.amount_minor - budget.available_minor, budget.amount_minor)
}

const canManage = computed(() => auth.isAdmin)

// 预算告警只是展示：提交时的额度校验仍由服务端条件 UPDATE 决定。
const alerts = computed(() =>
  items.value
    .map((budget) => {
      const used = budget.amount_minor - budget.available_minor
      return {
        name: budget.name,
        percent: budget.amount_minor > 0 ? Math.round((used * 100) / budget.amount_minor) : 0,
        over: budget.available_minor <= 0,
      }
    })
    .filter((a) => a.percent >= 90)
    .sort((a, b) => b.percent - a.percent),
)

const hasOverLimit = computed(() => alerts.value.some((a) => a.over))
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2>预算</h2>
        <p>提交费用时预占额度，审批通过后转为已支出；不同币种从不混算。</p>
      </div>
      <el-button v-if="canManage" type="primary" :icon="Plus" @click="openCreate">新建预算</el-button>
    </div>

    <el-alert
      v-if="alerts.length"
      :type="hasOverLimit ? 'error' : 'warning'"
      show-icon
      :closable="false"
      style="margin-bottom: 16px"
    >
      <template #title>
        {{ hasOverLimit ? '有预算已超限' : '有预算接近上限' }}（已用 ≥ 额度 90%）
      </template>
      <div>
        {{ alerts.map((a) => `${a.name}（${a.percent}%）`).join('、') }}
      </div>
    </el-alert>

    <div class="section-card">
      <div class="toolbar" style="margin-bottom: 14px">
        <el-select v-model="categoryFilter" style="width: 160px">
          <el-option value="" label="全部类别" />
          <el-option
            v-for="opt in categoryOptions"
            :key="opt.value"
            :value="opt.value"
            :label="opt.label"
          />
        </el-select>
        <span class="spacer" />
        <span class="muted">共 {{ total }} 条</span>
      </div>

      <el-table v-loading="loading" :data="items" stripe>
        <el-table-column label="名称" min-width="180">
          <template #default="{ row }">
            <div>{{ row.name }}</div>
            <div class="muted" style="font-size: 12px">
              {{ categoryLabel(row.category) }} · {{ formatDate(row.period_start) }} ~
              {{ formatDate(row.period_end) }}
            </div>
          </template>
        </el-table-column>
        <el-table-column label="额度" width="150" align="right">
          <template #default="{ row }">
            <span class="money">{{ formatMoney(row.amount_minor, row.currency) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="已预占" width="140" align="right">
          <template #default="{ row }">
            <span class="money muted">{{ formatMoney(row.reserved_minor, row.currency) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="已支出" width="140" align="right">
          <template #default="{ row }">
            <span class="money">{{ formatMoney(row.spent_minor, row.currency) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="可用" width="140" align="right">
          <template #default="{ row }">
            <span class="money" :style="{ color: row.available_minor <= 0 ? '#c45656' : '' }">
              {{ formatMoney(row.available_minor, row.currency) }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="使用率" width="160">
          <template #default="{ row }">
            <el-progress
              :percentage="usedPercent(row)"
              :stroke-width="8"
              :status="usedPercent(row) >= 100 ? 'exception' : undefined"
            />
          </template>
        </el-table-column>
        <el-table-column v-if="canManage" label="操作" width="140" align="right">
          <template #default="{ row }">
            <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
            <el-button link type="danger" @click="remove(row)">删除</el-button>
          </template>
        </el-table-column>
        <template #empty>
          <el-empty description="还没有预算" />
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

    <el-dialog
      v-model="dialogVisible"
      :title="editing ? '编辑预算' : '新建预算'"
      width="520px"
      destroy-on-close
    >
      <el-form ref="formRef" :model="form" :rules="rules" label-position="top">
        <el-form-item label="名称" prop="name">
          <el-input v-model="form.name" maxlength="120" placeholder="例如 2026 Q4 差旅" />
        </el-form-item>
        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="类别" prop="category">
              <el-select v-model="form.category" :disabled="!!editing" style="width: 100%">
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
            <el-form-item label="币种" prop="currency">
              <el-select v-model="form.currency" :disabled="!!editing" style="width: 100%">
                <el-option v-for="c in currencies" :key="c" :value="c" :label="c" />
              </el-select>
            </el-form-item>
          </el-col>
        </el-row>
        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="开始日期" prop="period_start">
              <el-date-picker
                v-model="form.period_start"
                type="date"
                value-format="YYYY-MM-DD"
                style="width: 100%"
              />
            </el-form-item>
          </el-col>
          <el-col :span="12">
            <el-form-item label="结束日期" prop="period_end">
              <el-date-picker
                v-model="form.period_end"
                type="date"
                value-format="YYYY-MM-DD"
                style="width: 100%"
              />
            </el-form-item>
          </el-col>
        </el-row>
        <el-form-item label="额度" prop="amount">
          <el-input v-model="form.amount" placeholder="例如 5000.00">
            <template #append>{{ form.currency }}</template>
          </el-input>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>
