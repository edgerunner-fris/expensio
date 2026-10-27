import { api, type Page } from './client'
import type {
  CreatedInvitation,
  Invitation,
  InvitationInfo,
  Member,
  Profile,
  Role,
  Session,
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
