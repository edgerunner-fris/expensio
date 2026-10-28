<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { apiMessage } from '@/api/client'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const router = useRouter()

const profileFormRef = ref<FormInstance>()
const profileForm = reactive({ display_name: auth.profile?.display_name ?? '' })
const profileSaving = ref(false)

const passwordFormRef = ref<FormInstance>()
const passwordForm = reactive({ current_password: '', new_password: '', confirm: '' })
const passwordSaving = ref(false)

// 长度按后端的 64 字符上限；表单校验只做即时提示，最终以服务端 400 为准。
const profileRules: FormRules = {
  display_name: [
    { required: true, message: '请输入显示名称', trigger: 'blur' },
    { max: 64, message: '显示名称不超过 64 个字符', trigger: 'blur' },
  ],
}

const passwordRules: FormRules = {
  current_password: [{ required: true, message: '请输入当前密码', trigger: 'blur' }],
  new_password: [
    { required: true, message: '请输入新密码', trigger: 'blur' },
    { min: 10, max: 128, message: '密码长度为 10–128 个字符', trigger: 'blur' },
  ],
  confirm: [
    { required: true, message: '请再次输入新密码', trigger: 'blur' },
    {
      validator: (_rule: unknown, value: string, callback: (error?: string | Error) => void) => {
        if (value !== passwordForm.new_password) callback(new Error('两次输入的新密码不一致'))
        else callback()
      },
      trigger: 'blur',
    },
  ],
}

async function saveProfile(): Promise<void> {
  const valid = await profileFormRef.value?.validate().catch(() => false)
  if (!valid) return
  const name = profileForm.display_name.trim()
  if (!name) {
    ElMessage.error('显示名称不能为空')
    return
  }
  profileSaving.value = true
  try {
    await auth.updateProfile(name)
    // 服务端会去掉首尾空白，输入框同步成最终值。
    profileForm.display_name = name
    ElMessage.success('资料已更新')
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    profileSaving.value = false
  }
}

async function changePassword(): Promise<void> {
  const valid = await passwordFormRef.value?.validate().catch(() => false)
  if (!valid) return
  passwordSaving.value = true
  try {
    await auth.changePassword(passwordForm.current_password, passwordForm.new_password)
    // 服务端已撤销全部刷新链；本地也立即清空，避免界面停留在"已登录"。
    await auth.logout()
    ElMessage.success('密码已修改，请重新登录')
    await router.replace({ name: 'login' })
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    passwordSaving.value = false
  }
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2>账号设置</h2>
        <p>账号在所有租户之间共享；角色与租户归属由管理员维护。</p>
      </div>
    </div>

    <div class="settings-grid">
      <div class="section-card">
        <h3>个人资料</h3>
        <el-form
          ref="profileFormRef"
          :model="profileForm"
          :rules="profileRules"
          label-width="96px"
          @submit.prevent
        >
          <el-form-item label="邮箱">
            <el-input :model-value="auth.profile?.email" disabled />
          </el-form-item>
          <el-form-item label="当前租户">
            <el-input :model-value="auth.profile?.tenant_name" disabled />
          </el-form-item>
          <el-form-item label="显示名称" prop="display_name">
            <el-input v-model="profileForm.display_name" maxlength="64" show-word-limit />
          </el-form-item>
          <el-form-item>
            <el-button type="primary" :loading="profileSaving" @click="saveProfile">
              保存
            </el-button>
          </el-form-item>
        </el-form>
      </div>

      <div class="section-card">
        <h3>修改密码</h3>
        <p class="muted hint">修改成功后，所有设备上的登录会话都会失效。</p>
        <el-form
          ref="passwordFormRef"
          :model="passwordForm"
          :rules="passwordRules"
          label-width="96px"
          @submit.prevent
        >
          <el-form-item label="当前密码" prop="current_password">
            <el-input
              v-model="passwordForm.current_password"
              type="password"
              show-password
              autocomplete="current-password"
            />
          </el-form-item>
          <el-form-item label="新密码" prop="new_password">
            <el-input
              v-model="passwordForm.new_password"
              type="password"
              show-password
              autocomplete="new-password"
            />
          </el-form-item>
          <el-form-item label="确认新密码" prop="confirm">
            <el-input
              v-model="passwordForm.confirm"
              type="password"
              show-password
              autocomplete="new-password"
            />
          </el-form-item>
          <el-form-item>
            <el-button type="primary" :loading="passwordSaving" @click="changePassword">
              修改密码
            </el-button>
          </el-form-item>
        </el-form>
      </div>
    </div>
  </div>
</template>

<style scoped>
.settings-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(340px, 1fr));
  gap: 16px;
  align-items: start;
}

.hint {
  margin: -4px 0 14px;
  font-size: 13px;
}
</style>
