<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { apiMessage } from '@/api/client'
import { authApi } from '@/api/auth'
import type { AuditEntry } from '@/api/types'
import { formatDateTime } from '@/utils/format'

const entries = ref<AuditEntry[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = 20
const loading = ref(false)

async function load(): Promise<void> {
  loading.value = true
  try {
    const result = await authApi.auditLogs(page.value, pageSize)
    entries.value = result.items
    total.value = result.total
  } catch (err) {
    // 普通成员在菜单上看不到入口，直接访问 URL 时服务端返回 403。
    ElMessage.error(apiMessage(err))
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void load()
})

function onPageChange(next: number): void {
  page.value = next
  void load()
}

function shortID(value?: string): string {
  if (!value) return '—'
  return value.length > 8 ? `${value.slice(0, 8)}…` : value
}

function target(row: AuditEntry): string {
  if (!row.target_type && !row.target_id) return '—'
  return `${row.target_type || '—'} / ${shortID(row.target_id)}`
}

function prettyMetadata(row: AuditEntry): string {
  const value = row.metadata ?? {}
  if (Object.keys(value).length === 0) return '无附加信息'
  return JSON.stringify(value, null, 2)
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2>审计日志</h2>
        <p>租户内所有敏感操作的追加记录；普通成员无权查看。</p>
      </div>
    </div>

    <div class="section-card">
      <el-table v-loading="loading" :data="entries" row-key="id">
        <el-table-column type="expand">
          <template #default="{ row }">
            <pre class="metadata">{{ prettyMetadata(row) }}</pre>
          </template>
        </el-table-column>
        <el-table-column label="时间" width="150">
          <template #default="{ row }">{{ formatDateTime(row.created_at) }}</template>
        </el-table-column>
        <el-table-column label="动作" min-width="180">
          <template #default="{ row }">
            <el-tag effect="plain" size="small">{{ row.action }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作者" width="110">
          <template #default="{ row }">
            <span class="mono">{{ shortID(row.actor_user_id) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="对象" min-width="170">
          <template #default="{ row }">
            <span class="mono">{{ target(row) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="来源 IP" width="130">
          <template #default="{ row }">
            <span class="mono">{{ row.ip || '—' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="客户端" min-width="180" show-overflow-tooltip>
          <template #default="{ row }">{{ row.user_agent || '—' }}</template>
        </el-table-column>
      </el-table>

      <div class="pager">
        <el-pagination
          layout="total, prev, pager, next"
          :total="total"
          :page-size="pageSize"
          :current-page="page"
          @current-change="onPageChange"
        />
      </div>
    </div>
  </div>
</template>

<style scoped>
.metadata {
  margin: 0;
  padding: 8px 16px 8px 48px;
  font-size: 12px;
  color: var(--expensio-muted);
  white-space: pre-wrap;
  word-break: break-all;
}

.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 12px;
}

.pager {
  display: flex;
  justify-content: flex-end;
  padding-top: 14px;
}
</style>
