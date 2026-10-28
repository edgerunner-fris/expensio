<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import {
  ArrowDown,
  Bell,
  Checked,
  CreditCard,
  DataAnalysis,
  Document,
  Files,
  Odometer,
  User,
  Wallet,
} from '@element-plus/icons-vue'
import { apiMessage } from '@/api/client'
import { useAuthStore } from '@/stores/auth'
import { useNotificationStore } from '@/stores/notifications'
import { roleLabel } from '@/utils/format'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const notifications = useNotificationStore()

const switching = ref(false)

onMounted(() => {
  void notifications.refreshUnread()
  // 租户列表失败不阻塞布局：顶部退化为只显示当前租户名。
  void auth.loadTenants().catch(() => undefined)
})

// 离开通知页时刷新未读数：通知页内的标记已读会直接更新 store。
watch(
  () => route.path,
  (path) => {
    if (path !== '/notifications') void notifications.refreshUnread()
  },
)

const activeMenu = computed(() => {
  if (route.path.startsWith('/expenses')) return '/expenses'
  return route.path
})

async function handleLogout(): Promise<void> {
  await auth.logout()
  ElMessage.success('已退出登录')
  await router.push({ name: 'login' })
}

async function handleUserCommand(command: string): Promise<void> {
  if (command === 'settings') {
    await router.push('/settings')
    return
  }
  await handleLogout()
}

function canSwitch(t: { status: string; tenant_status: string }): boolean {
  return t.status === 'active' && t.tenant_status === 'active'
}

async function handleSwitchTenant(tenantId: string): Promise<void> {
  if (switching.value) return
  switching.value = true
  try {
    await auth.switchTenant(tenantId)
    // 停在当前页面会让旧租户的数据留在屏幕上；回到费用单重新拉取。
    await router.push('/expenses')
    await notifications.refreshUnread()
    ElMessage.success(`已切换到 ${auth.profile?.tenant_name ?? ''}`)
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    switching.value = false
  }
}
</script>

<template>
  <el-container class="app-shell">
    <el-aside width="224px" class="app-aside">
      <div class="brand">
        <div class="brand-mark">E</div>
        <div>
          <div class="brand-name">Expensio</div>
          <div class="brand-sub">费用管理</div>
        </div>
      </div>

      <el-menu :default-active="activeMenu" router class="app-menu">
        <el-menu-item index="/dashboard">
          <el-icon><Odometer /></el-icon>
          <span>概览</span>
        </el-menu-item>
        <el-menu-item index="/expenses">
          <el-icon><Files /></el-icon>
          <span>费用单</span>
        </el-menu-item>
        <el-menu-item index="/budgets">
          <el-icon><Wallet /></el-icon>
          <span>预算</span>
        </el-menu-item>
        <el-menu-item v-if="auth.isAdmin" index="/approvals">
          <el-icon><Checked /></el-icon>
          <span>审批</span>
        </el-menu-item>
        <el-menu-item index="/reports">
          <el-icon><DataAnalysis /></el-icon>
          <span>报表</span>
        </el-menu-item>
        <el-menu-item index="/billing">
          <el-icon><CreditCard /></el-icon>
          <span>账单</span>
        </el-menu-item>
        <el-menu-item index="/notifications">
          <el-icon><Bell /></el-icon>
          <span>通知</span>
        </el-menu-item>
        <el-menu-item v-if="auth.isAdmin" index="/members">
          <el-icon><User /></el-icon>
          <span>成员</span>
        </el-menu-item>
        <el-menu-item v-if="auth.isAdmin" index="/audit-logs">
          <el-icon><Document /></el-icon>
          <span>审计日志</span>
        </el-menu-item>
      </el-menu>
    </el-aside>

    <el-container>
      <el-header class="app-header">
        <div class="tenant">
          <el-dropdown
            v-if="auth.tenants.length > 1"
            trigger="click"
            :disabled="switching"
            @command="handleSwitchTenant"
          >
            <span class="tenant-trigger">
              <span class="tenant-name">{{ auth.profile?.tenant_name }}</span>
              <el-icon :size="12"><ArrowDown /></el-icon>
            </span>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item
                  v-for="t in auth.tenants"
                  :key="t.tenant_id"
                  :command="t.tenant_id"
                  :disabled="t.current || !canSwitch(t)"
                >
                  <span class="tenant-option">
                    <span>{{ t.name }}</span>
                    <el-tag size="small" effect="plain" type="info">{{ roleLabel(t.role) }}</el-tag>
                  </span>
                </el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
          <span v-else class="tenant-name">{{ auth.profile?.tenant_name }}</span>
          <el-tag size="small" effect="plain">{{ roleLabel(auth.profile?.role ?? '') }}</el-tag>
        </div>
        <div class="header-actions">
          <el-badge :value="notifications.unread" :max="99" :hidden="notifications.unread === 0">
            <el-button text circle aria-label="通知" @click="router.push('/notifications')">
              <el-icon :size="18"><Bell /></el-icon>
            </el-button>
          </el-badge>

          <el-dropdown trigger="click" @command="handleUserCommand">
            <span class="user-trigger">
              <el-avatar :size="28">{{ auth.profile?.display_name?.slice(0, 1) }}</el-avatar>
              <span class="user-name">{{ auth.profile?.display_name }}</span>
            </span>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item command="settings">账号设置</el-dropdown-item>
                <el-dropdown-item command="logout" divided>退出登录</el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </div>
      </el-header>

      <el-main class="app-main">
        <!-- 切换租户后强制重建页面：停留在同一路径时 push 不会重新挂载，
             旧租户的列表数据就会留在屏幕上。 -->
        <router-view :key="auth.profile?.tenant_id" />
      </el-main>
    </el-container>
  </el-container>
</template>

<style scoped>
.app-shell {
  min-height: 100%;
}

.app-aside {
  background: #fff;
  border-right: 1px solid var(--expensio-border);
  display: flex;
  flex-direction: column;
}

.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 20px 18px;
}

.brand-mark {
  width: 36px;
  height: 36px;
  border-radius: 10px;
  background: var(--expensio-primary);
  color: #fff;
  display: grid;
  place-items: center;
  font-weight: 700;
  font-size: 18px;
}

.brand-name {
  font-weight: 700;
  font-size: 16px;
}

.brand-sub {
  font-size: 11px;
  color: var(--expensio-muted);
}

.app-menu {
  border-right: none;
  padding: 0 8px;
}

.app-menu :deep(.el-menu-item) {
  border-radius: 8px;
  margin-bottom: 4px;
}

.app-menu :deep(.el-menu-item.is-active) {
  background: #eaf1ff;
  color: var(--expensio-primary);
}

.app-header {
  height: 60px;
  background: #fff;
  border-bottom: 1px solid var(--expensio-border);
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 24px;
}

.tenant {
  display: flex;
  align-items: center;
  gap: 8px;
}

.tenant-name {
  font-weight: 600;
  font-size: 14px;
}

.tenant-trigger {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  cursor: pointer;
  outline: none;
}

.tenant-option {
  display: flex;
  align-items: center;
  gap: 12px;
  justify-content: space-between;
}

.header-actions {
  display: flex;
  align-items: center;
  gap: 18px;
}

.user-trigger {
  display: flex;
  align-items: center;
  gap: 8px;
  cursor: pointer;
  outline: none;
}

.user-name {
  font-size: 13px;
  color: var(--expensio-ink);
}

.app-main {
  padding: 24px;
  background: var(--expensio-bg);
}

@media (max-width: 860px) {
  .app-aside {
    width: 64px !important;
  }
  .brand > div:last-child,
  .app-menu span {
    display: none;
  }
  .brand {
    justify-content: center;
    padding: 16px 0;
  }
  .app-main {
    padding: 16px;
  }
}
</style>
