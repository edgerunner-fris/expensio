<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  ElMessage,
  ElMessageBox,
  type FormInstance,
  type FormRules,
} from 'element-plus'
import { ArrowLeft, Download } from '@element-plus/icons-vue'
import { apiMessage } from '@/api/client'
import { attachmentApi, budgetApi, commentApi, expenseApi } from '@/api/expenses'
import type {
  ApprovalProgress,
  Attachment,
  Budget,
  Category,
  Expense,
  ExpenseComment,
} from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import {
  categoryLabel,
  categoryOptions,
  currencies,
  formatDate,
  formatDateTime,
  formatMoney,
  minorToInput,
  occurredAtISO,
  parseMoneyToMinor,
  statusLabel,
  statusTagType,
} from '@/utils/format'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const expense = ref<Expense | null>(null)
const budget = ref<Budget | null>(null)
const loading = ref(false)
const acting = ref(false)

const mine = computed(() => expense.value?.submitter_id === auth.profile?.user_id)
const isDraft = computed(() => expense.value?.status === 'draft')
const isSubmitted = computed(() => expense.value?.status === 'submitted')

const approvals = ref<ApprovalProgress | null>(null)
const attachments = ref<Attachment[]>([])
const comments = ref<ExpenseComment[]>([])
const uploading = ref(false)
const commentDraft = ref('')
const commentSending = ref(false)

// 已经批过这张单的人不再看到"批准"按钮：后端会用唯一约束拒绝重复批准。
const alreadyApproved = computed(
  () => approvals.value?.items.some((a) => a.actor_id === auth.profile?.user_id) ?? false,
)

const canEdit = computed(() => isDraft.value && mine.value && auth.canWrite)
const canDelete = computed(() => isDraft.value && mine.value)
const canSubmit = computed(() => isDraft.value && mine.value && auth.canWrite)
const canDecide = computed(
  () => isSubmitted.value && auth.isAdmin && !mine.value && !alreadyApproved.value,
)
const canCancel = computed(() => isSubmitted.value && (mine.value || auth.isAdmin))
const canReimburse = computed(() => expense.value?.status === 'approved' && auth.isAdmin)
// 附件只在草稿阶段可写，且只有提交人可写：提交后它是审批证据。
const canManageAttachments = computed(() => isDraft.value && mine.value && auth.canWrite)
const canComment = computed(() => auth.canWrite)

async function load(): Promise<void> {
  const id = String(route.params.id)
  loading.value = true
  try {
    const found = await expenseApi.get(id)
    expense.value = found
    budget.value = null
    if (found.budget_id) {
      try {
        budget.value = await budgetApi.get(found.budget_id)
      } catch {
        // 预算可能已被删除，详情页不因此报错。
      }
    }
    // 协作数据互相独立：一个失败不应挡住详情页主体。
    const [progress, files, notes] = await Promise.allSettled([
      expenseApi.approvals(id),
      attachmentApi.list(id),
      commentApi.list(id),
    ])
    approvals.value = progress.status === 'fulfilled' ? progress.value : null
    attachments.value = files.status === 'fulfilled' ? files.value.items : []
    comments.value = notes.status === 'fulfilled' ? notes.value.items : []
  } catch (err) {
    ElMessage.error(apiMessage(err))
    await router.replace('/expenses')
  } finally {
    loading.value = false
  }
}

onMounted(load)
watch(() => route.params.id, load)

async function runAction(
  action: 'submit' | 'approve' | 'reject' | 'cancel' | 'reimburse',
  comment?: string,
): Promise<void> {
  if (!expense.value) return
  acting.value = true
  try {
    expense.value = await expenseApi.action(expense.value.id, action, comment)
    ElMessage.success('操作成功')
    await load()
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    acting.value = false
  }
}

async function submitExpense(): Promise<void> {
  await runAction('submit')
}

async function approve(): Promise<void> {
  await runAction('approve')
}

async function reject(): Promise<void> {
  try {
    const { value } = await ElMessageBox.prompt('请填写驳回理由，提交人会看到这段内容。', '驳回费用单', {
      confirmButtonText: '确认驳回',
      cancelButtonText: '取消',
      inputType: 'textarea',
      inputValidator: (v) => (v.trim() ? true : '驳回理由不能为空'),
    })
    await runAction('reject', value.trim())
  } catch {
    // 用户取消。
  }
}

async function cancel(): Promise<void> {
  try {
    await ElMessageBox.confirm('撤销后预算预占会被释放，且不能恢复。', '撤销费用单', {
      confirmButtonText: '确认撤销',
      cancelButtonText: '取消',
      type: 'warning',
    })
    await runAction('cancel')
  } catch {
    // 用户取消。
  }
}

async function reimburse(): Promise<void> {
  try {
    await ElMessageBox.confirm('确认这笔费用已经打款给提交人？', '标记已报销', {
      confirmButtonText: '确认已报销',
      cancelButtonText: '取消',
      type: 'warning',
    })
    await runAction('reimburse')
  } catch {
    // 用户取消。
  }
}

// ---- 编辑草稿 ----
const editVisible = ref(false)
const saving = ref(false)
const editRef = ref<FormInstance>()
const editForm = reactive({
  category: 'travel' as Category,
  currency: 'USD',
  amount: '',
  merchant: '',
  description: '',
  occurred_at: '',
  budget_id: '',
})
const editBudgets = ref<Budget[]>([])
const matchingBudgets = computed(() =>
  editBudgets.value.filter(
    (b) => b.category === editForm.category && b.currency === editForm.currency,
  ),
)

const editRules: FormRules = {
  amount: [
    { required: true, message: '请输入金额', trigger: 'blur' },
    {
      validator: (_rule, value, callback) => {
        if (parseMoneyToMinor(String(value), editForm.currency) === null) {
          callback(new Error('金额格式不正确'))
          return
        }
        callback()
      },
      trigger: 'blur',
    },
  ],
  merchant: [{ required: true, message: '请输入商户名称', trigger: 'blur' }],
  occurred_at: [{ required: true, message: '请选择发生日期', trigger: 'change' }],
}

async function openEdit(): Promise<void> {
  const e = expense.value
  if (!e) return
  editForm.category = e.category
  editForm.currency = e.currency
  editForm.amount = minorToInput(e.amount_minor, e.currency)
  editForm.merchant = e.merchant
  editForm.description = e.description
  editForm.occurred_at = formatDate(e.occurred_at)
  editForm.budget_id = e.budget_id ?? ''
  editVisible.value = true
  try {
    const result = await budgetApi.list({ page: 1, page_size: 100 })
    editBudgets.value = result.items
  } catch {
    editBudgets.value = []
  }
}

watch(
  () => [editForm.category, editForm.currency],
  () => {
    if (
      editBudgets.value.length > 0 &&
      editForm.budget_id &&
      !matchingBudgets.value.some((b) => b.id === editForm.budget_id)
    ) {
      editForm.budget_id = ''
    }
  },
)

async function saveEdit(): Promise<void> {
  const e = expense.value
  if (!e || !editRef.value) return
  const valid = await editRef.value.validate().catch(() => false)
  if (!valid) return
  const amountMinor = parseMoneyToMinor(editForm.amount, editForm.currency)
  if (amountMinor === null) return

  saving.value = true
  try {
    expense.value = await expenseApi.update(e.id, {
      version: e.version,
      category: editForm.category,
      currency: editForm.currency,
      amount_minor: amountMinor,
      merchant: editForm.merchant.trim(),
      description: editForm.description.trim(),
      occurred_at: occurredAtISO(editForm.occurred_at),
      ...(editForm.budget_id ? { budget_id: editForm.budget_id } : {}),
    })
    ElMessage.success('草稿已更新')
    editVisible.value = false
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    saving.value = false
  }
}

async function removeDraft(): Promise<void> {
  const e = expense.value
  if (!e) return
  try {
    await ElMessageBox.confirm('删除后这张草稿不可恢复。', '删除草稿', {
      confirmButtonText: '确认删除',
      cancelButtonText: '取消',
      type: 'warning',
    })
    await expenseApi.remove(e.id, e.version)
    ElMessage.success('草稿已删除')
    await router.replace('/expenses')
  } catch (err) {
    if (err !== 'cancel' && err !== 'close') ElMessage.error(apiMessage(err))
  }
}

// ---- 附件 ----
const fileInput = ref<HTMLInputElement>()

const allowedTypes: Record<string, string> = {
  'image/jpeg': '.jpg',
  'image/png': '.png',
  'image/webp': '.webp',
  'application/pdf': '.pdf',
}
const maxAttachmentBytes = 10 * 1024 * 1024

function formatBytes(size: number): string {
  if (size < 1024) return `${size} B`
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`
  return `${(size / 1024 / 1024).toFixed(1)} MB`
}

async function reloadAttachments(): Promise<void> {
  const e = expense.value
  if (!e) return
  try {
    attachments.value = (await attachmentApi.list(e.id)).items
  } catch {
    attachments.value = []
  }
}

async function onFilePicked(event: Event): Promise<void> {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  const e = expense.value
  if (!file || !e) return
  if (!allowedTypes[file.type]) {
    ElMessage.error('只支持 JPEG、PNG、WebP 或 PDF 附件')
    return
  }
  if (file.size === 0 || file.size > maxAttachmentBytes) {
    ElMessage.error('附件大小必须在 1 字节到 10 MiB 之间')
    return
  }

  uploading.value = true
  try {
    const presigned = await attachmentApi.presign(e.id, {
      file_name: file.name,
      content_type: file.type,
      size_bytes: file.size,
    })
    await attachmentApi.upload(presigned, file)
    await attachmentApi.confirm(e.id, {
      object_key: presigned.object_key,
      file_name: file.name,
      content_type: file.type,
      size_bytes: file.size,
    })
    ElMessage.success('附件已上传')
    await reloadAttachments()
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    uploading.value = false
  }
}

function downloadAttachment(a: Attachment): void {
  // 下载 URL 是对象存储的短期签名地址，不带 Authorization 头。
  window.open(a.download_url, '_blank', 'noopener')
}

async function removeAttachment(a: Attachment): Promise<void> {
  const e = expense.value
  if (!e) return
  try {
    await ElMessageBox.confirm(`删除附件「${a.file_name}」？`, '删除附件', {
      confirmButtonText: '删除',
      cancelButtonText: '取消',
      type: 'warning',
    })
    await attachmentApi.remove(e.id, a.id)
    ElMessage.success('附件已删除')
    await reloadAttachments()
  } catch (err) {
    if (err !== 'cancel' && err !== 'close') ElMessage.error(apiMessage(err))
  }
}

// ---- 评论 ----
function authorLabel(userID: string): string {
  if (userID === auth.profile?.user_id) return '我'
  return `成员 ${userID.slice(0, 8)}`
}

async function sendComment(): Promise<void> {
  const e = expense.value
  const body = commentDraft.value.trim()
  if (!e || !body) return
  commentSending.value = true
  try {
    await commentApi.add(e.id, body)
    commentDraft.value = ''
    comments.value = (await commentApi.list(e.id)).items
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    commentSending.value = false
  }
}

async function removeComment(comment: ExpenseComment): Promise<void> {
  const e = expense.value
  if (!e) return
  try {
    await commentApi.remove(e.id, comment.id)
    comments.value = (await commentApi.list(e.id)).items
  } catch (err) {
    ElMessage.error(apiMessage(err))
  }
}
</script>

<template>
  <div v-loading="loading" class="page">
    <div class="page-head">
      <div style="display: flex; align-items: center; gap: 12px">
        <el-button :icon="ArrowLeft" circle @click="router.push('/expenses')" />
        <div>
          <h2>费用详情</h2>
          <p v-if="expense">
            单号 <span class="money">{{ expense.id }}</span> · 版本 {{ expense.version }}
          </p>
        </div>
      </div>
      <div v-if="expense" class="toolbar">
        <el-button v-if="canEdit" @click="openEdit">编辑</el-button>
        <el-button v-if="canDelete" type="danger" plain @click="removeDraft">删除</el-button>
        <el-button v-if="canSubmit" type="primary" :loading="acting" @click="submitExpense">
          提交审批
        </el-button>
        <el-button v-if="canDecide" type="success" :loading="acting" @click="approve">批准</el-button>
        <el-button v-if="canDecide" type="danger" :loading="acting" @click="reject">驳回</el-button>
        <el-button v-if="canCancel" :loading="acting" @click="cancel">撤销</el-button>
        <el-button v-if="canReimburse" type="success" :loading="acting" @click="reimburse">
          标记已报销
        </el-button>
      </div>
    </div>

    <template v-if="expense">
      <el-alert
        v-if="expense.status === 'rejected' && expense.rejection_reason"
        type="error"
        :closable="false"
        show-icon
        :title="`已驳回：${expense.rejection_reason}`"
      />
      <el-alert
        v-if="isSubmitted && mine && !auth.isAdmin"
        type="info"
        :closable="false"
        show-icon
        title="等待管理员审批。审批期间这笔金额会占用对应预算的可用额度。"
      />

      <div class="section-card">
        <div style="display: flex; justify-content: space-between; gap: 16px; flex-wrap: wrap">
          <div>
            <div class="muted" style="font-size: 13px">{{ categoryLabel(expense.category) }} · {{ expense.merchant }}</div>
            <div style="font-size: 30px; font-weight: 700; margin-top: 6px" class="money">
              {{ formatMoney(expense.amount_minor, expense.currency) }}
            </div>
          </div>
          <el-tag :type="statusTagType(expense.status)" size="large">{{ statusLabel(expense.status) }}</el-tag>
        </div>

        <el-descriptions :column="2" border style="margin-top: 20px">
          <el-descriptions-item label="发生日期">{{ formatDate(expense.occurred_at) }}</el-descriptions-item>
          <el-descriptions-item label="类别">{{ categoryLabel(expense.category) }}</el-descriptions-item>
          <el-descriptions-item label="商户">{{ expense.merchant }}</el-descriptions-item>
          <el-descriptions-item label="关联预算">
            <template v-if="budget">
              {{ budget.name }}（{{ budget.currency }}，可用
              {{ formatMoney(budget.available_minor, budget.currency) }}）
            </template>
            <span v-else class="muted">未关联</span>
          </el-descriptions-item>
          <el-descriptions-item label="说明" :span="2">
            {{ expense.description || '—' }}
          </el-descriptions-item>
          <el-descriptions-item label="创建时间">{{ formatDateTime(expense.created_at) }}</el-descriptions-item>
          <el-descriptions-item label="更新时间">{{ formatDateTime(expense.updated_at) }}</el-descriptions-item>
          <el-descriptions-item label="审批人">
            {{ expense.decided_by ?? '—' }}
          </el-descriptions-item>
          <el-descriptions-item label="审批时间">{{ formatDateTime(expense.decided_at) }}</el-descriptions-item>
        </el-descriptions>
      </div>

      <div v-if="approvals && approvals.required_levels > 1" class="section-card">
        <div style="display: flex; justify-content: space-between; align-items: center">
          <h3 style="margin: 0">分级审批</h3>
          <el-tag effect="plain" :type="approvals.approved_levels >= approvals.required_levels ? 'success' : 'warning'">
            {{ approvals.approved_levels }} / {{ approvals.required_levels }} 级已批准
          </el-tag>
        </div>
        <el-timeline style="margin-top: 16px">
          <el-timeline-item
            v-for="item in approvals.items"
            :key="item.level"
            :timestamp="formatDateTime(item.created_at)"
          >
            第 {{ item.level }} 级 · {{ authorLabel(item.actor_id) }}
            <div v-if="item.comment" class="muted" style="white-space: pre-wrap">{{ item.comment }}</div>
          </el-timeline-item>
        </el-timeline>
        <p v-if="approvals.approved_levels < approvals.required_levels" class="muted" style="margin: 0">
          还需 {{ approvals.required_levels - approvals.approved_levels }} 位不同的管理员批准，全部完成后才会扣减预算。
        </p>
      </div>

      <div class="section-card">
        <div style="display: flex; justify-content: space-between; align-items: center">
          <h3 style="margin: 0">附件 <span class="muted" style="font-weight: 400">（{{ attachments.length }}/10）</span></h3>
          <div v-if="canManageAttachments">
            <input
              ref="fileInput"
              type="file"
              accept=".jpg,.jpeg,.png,.webp,.pdf"
              hidden
              @change="onFilePicked"
            />
            <el-button :loading="uploading" @click="fileInput?.click()">上传附件</el-button>
          </div>
        </div>
        <p v-if="attachments.length === 0" class="muted" style="margin: 8px 0 0">
          没有附件。{{ canManageAttachments ? '' : '附件只能在草稿阶段由提交人上传。' }}
        </p>
        <ul v-else class="thread-list">
          <li v-for="a in attachments" :key="a.id">
            <div>
              <div>{{ a.file_name }}</div>
              <div class="muted" style="font-size: 12px">
                {{ formatBytes(a.size_bytes) }} · {{ formatDateTime(a.created_at) }} · {{ authorLabel(a.uploaded_by) }}
              </div>
            </div>
            <div style="display: flex; gap: 4px; flex-shrink: 0">
              <el-button text :icon="Download" @click="downloadAttachment(a)">下载</el-button>
              <el-button v-if="canManageAttachments" text type="danger" @click="removeAttachment(a)">
                删除
              </el-button>
            </div>
          </li>
        </ul>
      </div>

      <div class="section-card">
        <h3>评论</h3>
        <ul v-if="comments.length > 0" class="thread-list">
          <li v-for="c in comments" :key="c.id">
            <div>
              <div style="font-weight: 600">
                {{ authorLabel(c.author_id) }}
                <span class="muted" style="font-weight: 400; font-size: 12px">
                  {{ formatDateTime(c.created_at) }}
                </span>
              </div>
              <div style="white-space: pre-wrap">{{ c.body }}</div>
            </div>
            <el-button
              v-if="c.author_id === auth.profile?.user_id || auth.isAdmin"
              text
              type="danger"
              @click="removeComment(c)"
            >
              删除
            </el-button>
          </li>
        </ul>
        <p v-else class="muted" style="margin: 0">还没有评论。</p>
        <div v-if="canComment" style="margin-top: 12px">
          <el-input
            v-model="commentDraft"
            type="textarea"
            :rows="2"
            maxlength="2000"
            show-word-limit
            placeholder="留言给审批人或其他成员…"
          />
          <div style="margin-top: 8px; text-align: right">
            <el-button
              type="primary"
              :loading="commentSending"
              :disabled="!commentDraft.trim()"
              @click="sendComment"
            >
              发送
            </el-button>
          </div>
        </div>
        <p v-else class="muted" style="margin: 8px 0 0">只读成员不能评论。</p>
      </div>
    </template>

    <el-dialog v-model="editVisible" title="编辑草稿" width="520px" destroy-on-close>
      <el-form ref="editRef" :model="editForm" :rules="editRules" label-position="top">
        <el-row :gutter="12">
          <el-col :span="12">
            <el-form-item label="类别" prop="category">
              <el-select v-model="editForm.category" style="width: 100%">
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
                v-model="editForm.occurred_at"
                type="date"
                value-format="YYYY-MM-DD"
                style="width: 100%"
              />
            </el-form-item>
          </el-col>
        </el-row>
        <el-form-item label="金额" prop="amount">
          <el-input v-model="editForm.amount">
            <template #append>
              <el-select v-model="editForm.currency" style="width: 90px">
                <el-option v-for="c in currencies" :key="c" :value="c" :label="c" />
              </el-select>
            </template>
          </el-input>
        </el-form-item>
        <el-form-item label="商户" prop="merchant">
          <el-input v-model="editForm.merchant" maxlength="200" />
        </el-form-item>
        <el-form-item label="说明">
          <el-input v-model="editForm.description" type="textarea" :rows="3" maxlength="2000" show-word-limit />
        </el-form-item>
        <el-form-item label="关联预算">
          <el-select v-model="editForm.budget_id" clearable style="width: 100%" placeholder="不关联预算">
            <el-option
              v-for="b in matchingBudgets"
              :key="b.id"
              :value="b.id"
              :label="`${b.name}（可用 ${formatMoney(b.available_minor, b.currency)}）`"
            />
          </el-select>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="editVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="saveEdit">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.thread-list {
  list-style: none;
  margin: 12px 0 0;
  padding: 0;
}

.thread-list li {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  padding: 10px 0;
  border-top: 1px solid var(--expensio-border);
}

.thread-list li:first-child {
  border-top: none;
}
</style>
