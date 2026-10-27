<script setup lang="ts">
import { useRouter } from 'vue-router'

const router = useRouter()
</script>

<template>
  <div class="auth-shell">
    <div class="auth-card" style="text-align: center">
      <h1>404</h1>
      <p class="subtitle">这个页面不存在，或者已被移动。</p>
      <el-button type="primary" @click="router.push('/expenses')">回到费用单</el-button>
    </div>
  </div>
</template>
