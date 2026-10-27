<script setup lang="ts">
import { onMounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { notificationApi } from '@/api/notifications'
import type { NotificationItem } from '@/api/types'
import { useNotificationStore } from '@/stores/notifications'
import { apiMessage } from '@/api/client'
import { formatDateTime } from '@/utils/format'

const router = useRouter()
const notifications = useNotificationStore()

const filters = reactive<{ unreadOnly: boolean }>({ unreadOnly: false })
const paging = reactive({ page: 1, pageSize: 20, total: 0 })
const items = ref<NotificationItem[]>([])
const loading = ref(false)

async function load(): Promise<void> {
  loading.value = true
  try {
    const page = await notificationApi.list(filters.unreadOnly, paging.page, paging.pageSize)
    items.value = page.items
    paging.total = page.total
    paging.page = page.page
  } catch (err) {
    ElMessage.error(apiMessage(err))
  } finally {
    loading.value = false
  }
}

onMounted(load)

watch(
  () => filters.unreadOnly,
  () => {
    paging.page = 1
    void load()
  },
)

async function markRead(item: NotificationItem): Promise<void> {
  if (item.read_at) return
  try {
    await notificationApi.markRead(item.id)
    item.read_at = new Date().toISOString()
    notifications.decrement()
  } catch (err) {
    ElMessage.error(apiMessage(err))
  }
}

async function open(item: NotificationItem): Promise<void> {
  await markRead(item)
  if (item.resource_type === 'expense' && item.resource_id) {
    await router.push(`/expenses/${item.resource_id}`)
    return
  }
  if (item.resource_type) await router.push('/billing')
}

async function markAllRead(): Promise<void> {
  try {
    const result = await notificationApi.markAllRead()
    notifications.setUnread(0)
    ElMessage.success(`已将 ${result.updated} 条通知标为已读`)
    await load()
  } catch (err) {
    ElMessage.error(apiMessage(err))
  }
}

function changePage(page: number): void {
  paging.page = page
  void load()
}
</script>

<template>
  <div>
    <div class="page-header">
      <div>
        <h1>通知</h1>
        <p class="muted">审批、报销与账单提醒；通知只是提示，业务事实仍以费用单/账单为准。</p>
      </div>
      <div class="header-actions">
        <el-switch v-model="filters.unreadOnly" active-text="只看未读" />
        <el-button :disabled="notifications.unread === 0" @click="markAllRead">
          全部标为已读
        </el-button>
      </div>
    </div>

    <el-table v-loading="loading" :data="items" empty-text="暂无通知">
      <el-table-column width="40">
        <template #default="{ row }">
          <span class="unread-dot" :class="{ active: !row.read_at }" />
        </template>
      </el-table-column>
      <el-table-column label="通知" min-width="360">
        <template #default="{ row }">
          <div class="notice-title" :class="{ unread: !row.read_at }">{{ row.title }}</div>
          <div class="notice-body">{{ row.body }}</div>
        </template>
      </el-table-column>
      <el-table-column label="时间" width="170">
        <template #default="{ row }">{{ formatDateTime(row.created_at) }}</template>
      </el-table-column>
      <el-table-column label="操作" width="160">
        <template #default="{ row }">
          <el-button v-if="!row.read_at" link type="primary" @click="markRead(row)">
            标为已读
          </el-button>
          <el-button v-if="row.resource_type" link @click="open(row)">查看</el-button>
        </template>
      </el-table-column>
    </el-table>

    <div class="pager">
      <el-pagination
        background
        layout="total, prev, pager, next"
        :total="paging.total"
        :page-size="paging.pageSize"
        :current-page="paging.page"
        @current-change="changePage"
      />
    </div>
  </div>
</template>

<style scoped>
.page-header {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 16px;
  margin-bottom: 18px;
}

.page-header h1 {
  margin: 0 0 4px;
  font-size: 22px;
}

.muted {
  margin: 0;
  color: var(--expensio-muted);
  font-size: 13px;
}

.header-actions {
  display: flex;
  align-items: center;
  gap: 14px;
}

.unread-dot {
  display: inline-block;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: transparent;
}

.unread-dot.active {
  background: var(--expensio-primary);
}

.notice-title {
  font-weight: 500;
  color: var(--expensio-ink);
}

.notice-title.unread {
  font-weight: 650;
}

.notice-body {
  margin-top: 2px;
  font-size: 12px;
  color: var(--expensio-muted);
}

.pager {
  display: flex;
  justify-content: flex-end;
  margin-top: 16px;
}

@media (max-width: 860px) {
  .page-header {
    flex-direction: column;
  }
}
</style>
