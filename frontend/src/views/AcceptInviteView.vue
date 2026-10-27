<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { apiMessage } from '@/api/client'
import { authApi } from '@/api/auth'
import type { InvitationInfo } from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import { formatDateTime, roleLabel } from '@/utils/format'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const token = String(route.params.token ?? '')

const info = ref<InvitationInfo | null>(null)
const loadError = ref('')
const loading = ref(true)
const formRef = ref<FormInstance>()
const form = reactive({ password: '', display_name: '' })

// 已有账号时只校验"密码非空"：密码长度规则属于注册，不该拿来
// 拒绝一个本来就能登录的旧账号。
const rules = computed<FormRules>(() => ({
  password: info.value?.account_exists
    ? [{ required: true, message: '请输入该账号的密码', trigger: 'blur' }]
    : [
        { required: true, message: '请设置密码', trigger: 'blur' },
        { min: 10, max: 128, message: '密码长度需在 10 到 128 个字符之间', trigger: 'blur' },
      ],
}))

onMounted(async () => {
  try {
    info.value = await authApi.invitation(token)
  } catch (err) {
    loadError.value = apiMessage(err)
  } finally {
    loading.value = false
  }
})

async function submit(): Promise<void> {
  if (!formRef.value || !info.value) return
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) return
  try {
    await auth.acceptInvitation(token, {
      password: form.password,
      display_name: form.display_name.trim() || undefined,
    })
    ElMessage.success(`已加入 ${info.value.tenant_name}`)
    await router.replace('/expenses')
  } catch (err) {
    ElMessage.error(apiMessage(err))
  }
}
</script>

<template>
  <div class="auth-shell">
    <div class="auth-card">
      <div class="auth-brand">
        <div class="auth-brand-mark">E</div>
        <span style="font-weight: 700">Expensio</span>
      </div>

      <template v-if="loading">
        <h1>正在校验邀请链接</h1>
        <p class="subtitle">请稍候……</p>
      </template>

      <template v-else-if="loadError">
        <h1>邀请不可用</h1>
        <el-alert :title="loadError" type="error" :closable="false" show-icon />
        <div class="auth-footer">
          <router-link to="/login">返回登录</router-link>
        </div>
      </template>

      <template v-else-if="info">
        <h1>加入 {{ info.tenant_name }}</h1>
        <p class="subtitle">
          邀请发送至 {{ info.email }}，角色为{{ roleLabel(info.role) }}。<br />
          链接有效期至 {{ formatDateTime(info.expires_at) }}。
        </p>

        <el-form
          ref="formRef"
          :model="form"
          :rules="rules"
          label-position="top"
          @submit.prevent="submit"
        >
          <el-form-item v-if="!info.account_exists" label="你的姓名" prop="display_name">
            <el-input v-model="form.display_name" placeholder="例如 李明" autocomplete="name" />
          </el-form-item>
          <el-form-item :label="info.account_exists ? '账号密码' : '设置密码'" prop="password">
            <el-input
              v-model="form.password"
              type="password"
              show-password
              :autocomplete="info.account_exists ? 'current-password' : 'new-password'"
              @keyup.enter="submit"
            />
          </el-form-item>

          <el-alert
            v-if="info.account_exists"
            type="info"
            :closable="false"
            title="该邮箱已有 Expensio 账号，请输入原密码确认身份。"
            style="margin-bottom: 16px"
          />

          <el-button type="primary" native-type="submit" :loading="auth.busy" style="width: 100%">
            {{ info.account_exists ? '确认加入' : '设置密码并加入' }}
          </el-button>
        </el-form>

        <div class="auth-footer">加入后，当前浏览器会切换到该租户的会话。</div>
      </template>
    </div>
  </div>
</template>
