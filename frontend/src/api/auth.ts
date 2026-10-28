import { api, type Page } from './client'
import type {
  AuditEntry,
  CreatedInvitation,
  Invitation,
  InvitationInfo,
  Member,
  Profile,
  Role,
  Session,
  TenantMembership,
} from './types'

export interface RegisterPayload {
  tenant_slug: string
  tenant_name: string
  email: string
  password: string
  display_name?: string
}

export interface LoginPayload {
  tenant_slug: string
  email: string
  password: string
}

export const authApi = {
  register(payload: RegisterPayload) {
    return api.post<Session>('/auth/register', payload)
  },
  login(payload: LoginPayload) {
    return api.post<Session>('/auth/login', payload)
  },
  logout(refreshToken: string) {
    return api.post<void>('/auth/logout', { refresh_token: refreshToken })
  },
  me() {
    return api.get<Profile>('/me')
  },
  // 租户列表是"我的成员关系"，跨租户但只限本人；服务端靠
  // memberships 的 self_membership_read 策略放行。
  async tenants() {
    const result = await api.get<{ items: TenantMembership[] }>('/auth/tenants')
    return result.items
  },
  switchTenant(tenantId: string) {
    return api.post<Session>('/auth/switch-tenant', { tenant_id: tenantId })
  },
  updateProfile(displayName: string) {
    return api.patch<Profile>('/me', { display_name: displayName })
  },
  changePassword(currentPassword: string, newPassword: string) {
    return api.post<void>('/auth/change-password', {
      current_password: currentPassword,
      new_password: newPassword,
    })
  },
  auditLogs(page = 1, pageSize = 20) {
    return api.get<Page<AuditEntry>>('/audit-logs', {
      params: { page, page_size: pageSize },
    })
  },
  members(page = 1, pageSize = 50) {
    return api.get<Page<Member>>('/tenants/current/members', {
      params: { page, page_size: pageSize },
    })
  },
  updateMemberRole(userId: string, role: string) {
    return api.patch<Member>(`/tenants/current/members/${userId}`, { role })
  },
  invitations(page = 1, pageSize = 50) {
    return api.get<Page<Invitation>>('/tenants/current/invitations', {
      params: { page, page_size: pageSize },
    })
  },
  createInvitation(payload: { email: string; role: Role }) {
    return api.post<CreatedInvitation>('/tenants/current/invitations', payload)
  },
  revokeInvitation(invitationId: string) {
    return api.delete<void>(`/tenants/current/invitations/${invitationId}`)
  },
  invitation(token: string) {
    return api.get<InvitationInfo>(`/invitations/${encodeURIComponent(token)}`)
  },
  acceptInvitation(token: string, payload: { password: string; display_name?: string }) {
    return api.post<Session>(`/invitations/${encodeURIComponent(token)}/accept`, payload)
  },
}
