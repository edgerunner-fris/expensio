import { defineStore } from 'pinia'
import { ref } from 'vue'
import { notificationApi } from '@/api/notifications'

// 未读数由布局与通知页共享：任何一次标记已读都要立刻反映到铃铛上，
// 轮询失败时保留旧值，不把导航栏变成错误提示区。
export const useNotificationStore = defineStore('notifications', () => {
  const unread = ref(0)

  async function refreshUnread(): Promise<void> {
    try {
      const result = await notificationApi.unreadCount()
      unread.value = result.unread
    } catch {
      // 布局层不发提示；页面本身的请求失败会各有各的错误处理。
    }
  }

  function setUnread(value: number): void {
    unread.value = Math.max(0, value)
  }

  function decrement(by = 1): void {
    unread.value = Math.max(0, unread.value - by)
  }

  return { unread, refreshUnread, setUnread, decrement }
})
