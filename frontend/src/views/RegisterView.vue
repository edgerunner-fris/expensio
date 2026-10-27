<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { apiMessage } from '@/api/client'
import { useAuthStore } from '@/stores/auth'

const router = useRouter()
const auth = useAuthStore()

const formRef = ref<FormInstance>()
const form = reactive({
  tenant_slug: '',
  tenant_name: '',
  email: '',
  display_name: '',
  password: '',
  confirm: '',
})

const rules: FormRules = {
  tenant_slug: [
    { required: true, message: '请输入租户标识', trigger: 'blur' },
    {
      pattern: /^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$/,
      message: '只能包含小写字母、数字与连字符，且以字母或数字开头结尾',
      trigger: 'blur',
    },
  ],
  tenant_name: [{ required: true, message: '请输入租户名称', trigger: 'blur' }],
  email: [
    { required: true, message: '请输入邮箱', trigger: 'blur' },
    { type: 'email', message: '邮箱格式不正确', trigger: 'blur' },
  ],
  password: [
    { required: true, message: '请输入密码', trigger: 'blur' },
    { min: 10, max: 128, message: '密码长度需在 10 到 128 个字符之间', trigger: 'blur' },
  ],
  confirm: [
    { required: true, message: '请再次输入密码', trigger: 'blur' },
    {
      validator: (_rule, value, callback) => {
        if (value !== form.password) callback(new Error('两次输入的密码不一致'))
        else callback()
      },
      trigger: 'blur',
    },
  ],
}

async function submit(): Promise<void> {
  if (!formRef.value) return
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) return
  try {
    await auth.register({
      tenant_slug: form.tenant_slug.trim(),
      tenant_name: form.tenant_name.trim(),
      email: form.email.trim(),
      display_name: form.display_name.trim(),
      password: form.password,
    })
    ElMessage.success('租户已创建，你已成为所有者')
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
      <h1>创建租户</h1>
      <p class="subtitle">注册后你是该租户的 owner，可邀请同事并分配角色</p>

      <el-form ref="formRef" :model="form" :rules="rules" label-position="top" @submit.prevent="submit">
        <el-form-item label="租户标识" prop="tenant_slug">
          <el-input v-model="form.tenant_slug" placeholder="例如 acme" autocomplete="organization" />
        </el-form-item>
        <el-form-item label="租户名称" prop="tenant_name">
          <el-input v-model="form.tenant_name" placeholder="例如 Acme 科技" />
        </el-form-item>
        <el-form-item label="你的姓名" prop="display_name">
          <el-input v-model="form.display_name" placeholder="例如 李明" autocomplete="name" />
        </el-form-item>
        <el-form-item label="邮箱" prop="email">
          <el-input v-model="form.email" placeholder="you@company.com" autocomplete="username" />
        </el-form-item>
        <el-form-item label="密码" prop="password">
          <el-input v-model="form.password" type="password" show-password autocomplete="new-password" />
        </el-form-item>
        <el-form-item label="确认密码" prop="confirm">
          <el-input
            v-model="form.confirm"
            type="password"
            show-password
            autocomplete="new-password"
            @keyup.enter="submit"
          />
        </el-form-item>
        <el-button type="primary" native-type="submit" :loading="auth.busy" style="width: 100%">
          创建并登录
        </el-button>
      </el-form>

      <div class="auth-footer">
        已有账号？<router-link to="/login">返回登录</router-link>
      </div>
    </div>
  </div>
</template>
