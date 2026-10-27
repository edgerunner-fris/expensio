import { api, type Page } from './client'
import type { NotificationItem } from './types'

export const notificationApi = {
  list(unreadOnly = false, page = 1, pageSize = 20) {
    return api.get<Page<NotificationItem>>('/notifications', {
      params: {
        unread_only: unreadOnly || undefined,
        page,
        page_size: pageSize,
      },
    })
  },
  unreadCount() {
    return api.get<{ unread: number }>('/notifications/unread-count')
  },
  markRead(id: string) {
    return api.post<void>(`/notifications/${id}/read`)
  },
  markAllRead() {
    return api.post<{ updated: number }>('/notifications/read-all')
  },
}
