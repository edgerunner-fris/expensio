<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { apiMessage } from '@/api/client'
import { useAuthStore } from '@/stores/auth'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const formRef = ref<FormInstance>()
const form = reactive({
  tenant_slug: '',
  email: '',
  password: '',
})

const rules: FormRules = {
  tenant_slug: [{ required: true, message: '请输入租户标识', trigger: 'blur' }],
  email: [{ required: true, message: '请输入邮箱', trigger: 'blur' }],
  password: [{ required: true, message: '请输入密码', trigger: 'blur' }],
}

async function submit(): Promise<void> {
  if (!formRef.value) return
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) return
  try {
    await auth.login({
      tenant_slug: form.tenant_slug.trim(),
      email: form.email.trim(),
      password: form.password,
    })
    ElMessage.success(`欢迎回来，${auth.profile?.display_name ?? ''}`)
    const redirect = typeof route.query.redirect === 'string' ? route.query.redirect : '/expenses'
    await router.replace(redirect)
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
      <h1>登录管理台</h1>
      <p class="subtitle">使用租户标识、邮箱与密码登录</p>

      <el-form ref="formRef" :model="form" :rules="rules" label-position="top" @submit.prevent="submit">
        <el-form-item label="租户标识" prop="tenant_slug">
          <el-input v-model="form.tenant_slug" placeholder="例如 acme" autocomplete="organization" />
        </el-form-item>
        <el-form-item label="邮箱" prop="email">
          <el-input v-model="form.email" placeholder="you@company.com" autocomplete="username" />
        </el-form-item>
        <el-form-item label="密码" prop="password">
          <el-input
            v-model="form.password"
            type="password"
            show-password
            autocomplete="current-password"
            @keyup.enter="submit"
          />
        </el-form-item>
        <el-button type="primary" native-type="submit" :loading="auth.busy" style="width: 100%">
          登录
        </el-button>
      </el-form>

      <div class="auth-footer">
        还没有账号？<router-link to="/register">创建租户</router-link>
      </div>
    </div>
  </div>
</template>
