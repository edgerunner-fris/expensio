<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { apiMessage } from '@/api/client'
import { authApi } from '@/api/auth'
import type { CreatedInvitation, Invitation, Member, Role } from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import { formatDate, formatDateTime, roleLabel } from '@/utils/format'

const auth = useAuthStore()

const members = ref<Member[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = 20
const loading = ref(false)
const pendingId = ref('')

const invitations = ref<Invitation[]>([])
const inviteLoading = ref(false)

const inviteDialog = ref(false)
const resultDialog = ref(false)
const created = ref<CreatedInvitation | null>(null)
const submitting = ref(false)
const inviteFormRef = ref<FormInstance>()
const inviteForm = reactive<{ email: string; role: Role }>({ email: '', role: 'member' })

const roleOptions: { value: Role; label: string }[] = [
  { value: 'owner', label: '所有者' },
  { value: 'admin', label: '管理员' },
  { value: 'member', label: '成员' },
  { value: 'viewer', label: '只读' },
]

// 与后端规则一致：非 owner 不能邀请 owner。
const invitableRoles = computed(() =>
  auth.isOwner ? roleOptions : roleOptions.filter((r) => r.value !== 'owner'),
)

const inviteRules: FormRules = {
  email: [
    { required: true, message: '请输入邮箱', trigger: 'blur' },
    { type: 'email', message: '邮箱格式不正确', trigger: 'blur' },
  ],
  role: [{ required: true, message: '请选择角色', trigger: 'change' }],
}

async function load(): Promise<void> {
  loading.value = true
  try {
    const result = await authApi.members(page.value, pageSize)
    members.value = result.items
    total.value = result.total
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    loading.value = false
  }
}

async function loadInvitations(): Promise<void> {
  inviteLoading.value = true
  try {
    const result = await authApi.invitations()
    invitations.value = result.items
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    inviteLoading.value = false
  }
}

onMounted(() => {
  void load()
  void loadInvitations()
})

function onPageChange(next: number): void {
  page.value = next
  void load()
}

function isSelf(member: Member): boolean {
  return member.user_id === auth.profile?.user_id
}

function canEdit(member: Member): boolean {
  if (isSelf(member)) return false
  // 与后端规则一致：非 owner 不能修改 owner 的角色。
  if (member.role === 'owner' && !auth.isOwner) return false
  return true
}

function availableRoles(member: Member): { value: Role; label: string }[] {
  if (auth.isOwner) return roleOptions
  return roleOptions.filter((r) => r.value !== 'owner' || member.role === 'owner')
}

async function changeRole(member: Member, next: string): Promise<void> {
  if (next === member.role) return
  pendingId.value = member.user_id
  try {
    const updated = await authApi.updateMemberRole(member.user_id, next)
    Object.assign(member, updated)
    ElMessage.success(`${member.display_name} 的角色已更新为${roleLabel(next)}`)
  } catch (err) {
    ElMessage.error(apiMessage(err))
    await load()
  } finally {
    pendingId.value = ''
  }
}

function openInvite(): void {
  inviteForm.email = ''
  inviteForm.role = 'member'
  inviteDialog.value = true
}

async function submitInvite(): Promise<void> {
  if (!inviteFormRef.value) return
  const valid = await inviteFormRef.value.validate().catch(() => false)
  if (!valid) return
  submitting.value = true
  try {
    created.value = await authApi.createInvitation({
      email: inviteForm.email.trim(),
      role: inviteForm.role,
    })
    inviteDialog.value = false
    resultDialog.value = true
    await loadInvitations()
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    submitting.value = false
  }
}

async function copyLink(): Promise<void> {
  if (!created.value) return
  try {
    await navigator.clipboard.writeText(created.value.link)
    ElMessage.success('链接已复制')
  } catch {
    ElMessage.error('复制失败，请手动选择链接复制')
  }
}

function isExpired(inv: Invitation): boolean {
  return new Date(inv.expires_at).getTime() <= Date.now()
}

async function revoke(inv: Invitation): Promise<void> {
  try {
    await ElMessageBox.confirm(
      `撤销后，发给 ${inv.email} 的链接将立即失效。`,
      '撤销邀请',
      { confirmButtonText: '撤销', cancelButtonText: '取消', type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await authApi.revokeInvitation(inv.id)
    ElMessage.success('邀请已撤销')
    await loadInvitations()
  } catch (err) {
    ElMessage.error(apiMessage(err))
  }
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2>成员</h2>
        <p>owner 可以授予 owner；管理员可以管理其他非 owner 成员；不能修改自己的角色。</p>
      </div>
      <el-button type="primary" @click="openInvite">邀请成员</el-button>
    </div>

    <div class="section-card">
      <el-table v-loading="loading" :data="members" stripe>
        <el-table-column label="成员" min-width="220">
          <template #default="{ row }">
            <div>{{ row.display_name }} <span v-if="isSelf(row)" class="muted">（本人）</span></div>
            <div class="muted" style="font-size: 12px">{{ row.email }}</div>
          </template>
        </el-table-column>
        <el-table-column label="角色" width="200">
          <template #default="{ row }">
            <el-select
              :model-value="row.role"
              size="small"
              :disabled="!canEdit(row) || pendingId === row.user_id"
              @change="changeRole(row, $event)"
            >
              <el-option
                v-for="opt in availableRoles(row)"
                :key="opt.value"
                :value="opt.value"
                :label="opt.label"
              />
            </el-select>
          </template>
        </el-table-column>
        <el-table-column label="账号状态" width="120">
          <template #default="{ row }">
            <el-tag size="small" :type="row.status === 'active' ? 'success' : 'info'">
              {{ row.status }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="加入时间" width="140">
          <template #default="{ row }">{{ formatDate(row.created_at) }}</template>
        </el-table-column>
        <template #empty>
          <el-empty description="暂无成员" />
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

    <div class="section-card" style="margin-top: 18px">
      <div class="page-head" style="margin-bottom: 10px">
        <div>
          <h3 style="margin: 0">待接受的邀请</h3>
          <p class="muted" style="margin: 4px 0 0">
            再次邀请同一邮箱会让旧链接立即失效；已过期的邀请可以撤销。
          </p>
        </div>
      </div>

      <el-table v-loading="inviteLoading" :data="invitations" stripe>
        <el-table-column label="邮箱" prop="email" min-width="220" />
        <el-table-column label="角色" width="110">
          <template #default="{ row }">{{ roleLabel(row.role) }}</template>
        </el-table-column>
        <el-table-column label="状态" width="150">
          <template #default="{ row }">
            <el-tag size="small" :type="isExpired(row) ? 'info' : 'success'">
              {{ isExpired(row) ? '已过期' : '等待接受' }}
            </el-tag>
            <div class="muted" style="font-size: 12px">
              至 {{ formatDateTime(row.expires_at) }}
            </div>
          </template>
        </el-table-column>
        <el-table-column label="创建时间" width="140">
          <template #default="{ row }">{{ formatDate(row.created_at) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="100">
          <template #default="{ row }">
            <el-button link type="danger" size="small" @click="revoke(row)">撤销</el-button>
          </template>
        </el-table-column>
        <template #empty>
          <el-empty description="暂无待接受的邀请" />
        </template>
      </el-table>
    </div>

    <el-dialog v-model="inviteDialog" title="邀请成员" width="480px">
      <el-form
        ref="inviteFormRef"
        :model="inviteForm"
        :rules="inviteRules"
        label-position="top"
        @submit.prevent="submitInvite"
      >
        <el-form-item label="邮箱" prop="email">
          <el-input v-model="inviteForm.email" placeholder="teammate@company.com" autocomplete="off" />
        </el-form-item>
        <el-form-item label="角色" prop="role">
          <el-select v-model="inviteForm.role" style="width: 100%">
            <el-option
              v-for="opt in invitableRoles"
              :key="opt.value"
              :value="opt.value"
              :label="opt.label"
            />
          </el-select>
        </el-form-item>
        <el-alert
          type="info"
          :closable="false"
          title="邀请邮件里包含一次性链接；创建后也可以直接复制链接发给对方。"
        />
      </el-form>
      <template #footer>
        <el-button @click="inviteDialog = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="submitInvite">发送邀请</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="resultDialog" title="邀请已创建" width="560px">
      <template v-if="created">
        <el-alert
          v-if="created.email_queued"
          type="success"
          :closable="false"
          title="邀请邮件已排队发送"
        />
        <el-alert
          v-else
          type="warning"
          :closable="false"
          title="邮件未能入队，请复制下面的链接手动发送"
        />
        <p class="muted" style="margin: 14px 0 6px">
          {{ created.email }}（{{ roleLabel(created.role) }}）
        </p>
        <el-input :model-value="created.link" readonly>
          <template #append>
            <el-button @click="copyLink">复制</el-button>
          </template>
        </el-input>
        <p class="muted" style="margin-top: 8px">
          链接只显示这一次，有效期至 {{ formatDateTime(created.expires_at) }}。
        </p>
      </template>
      <template #footer>
        <el-button type="primary" @click="resultDialog = false">完成</el-button>
      </template>
    </el-dialog>
  </div>
</template>
