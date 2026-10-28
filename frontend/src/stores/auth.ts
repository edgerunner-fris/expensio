import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { authApi, type LoginPayload, type RegisterPayload } from '@/api/auth'
import {
  accessToken,
  clearSession,
  loadProfile,
  refreshToken as storedRefreshToken,
  saveProfile,
  saveSession,
} from '@/api/session'
import type { Profile, Session, TenantMembership } from '@/api/types'

export const useAuthStore = defineStore('auth', () => {
  const profile = ref<Profile | null>(loadProfile())
  const busy = ref(false)
  // 当前账号的全部成员关系。为空时顶部只显示当前租户名，
  // 不影响其他功能。
  const tenants = ref<TenantMembership[]>([])

  const isAuthenticated = computed(() => profile.value !== null && accessToken() !== null)
  const isAdmin = computed(
    () => profile.value?.role === 'owner' || profile.value?.role === 'admin',
  )
  const isOwner = computed(() => profile.value?.role === 'owner')
  // viewer 是只读角色：可以看列表和报表，但不能建费用单。
  const canWrite = computed(
    () => profile.value !== null && profile.value.role !== 'viewer',
  )

  function adopt(session: Session): void {
    saveSession(session)
    profile.value = session.profile
  }

  async function login(payload: LoginPayload): Promise<void> {
    busy.value = true
    try {
      adopt(await authApi.login(payload))
    } finally {
      busy.value = false
    }
  }

  async function register(payload: RegisterPayload): Promise<void> {
    busy.value = true
    try {
      adopt(await authApi.register(payload))
    } finally {
      busy.value = false
    }
  }

  // 刷新页面后本地只有缓存的 profile；用它校验令牌是否仍然有效，
  // 顺便拿到管理员刚改过的角色。
  async function refreshProfile(): Promise<void> {
    if (!accessToken()) return
    const latest = await authApi.me()
    profile.value = latest
    saveProfile(latest)
  }

  async function loadTenants(): Promise<void> {
    tenants.value = await authApi.tenants()
  }

  // 切换租户会拿到一整套新会话（新的刷新令牌链）。旧链服务端不撤销，
  // 但那枚令牌已经不在本浏览器的 localStorage 里：切换以最后一个动作为准。
  async function switchTenant(tenantId: string): Promise<void> {
    if (tenantId === profile.value?.tenant_id) return
    busy.value = true
    try {
      adopt(await authApi.switchTenant(tenantId))
    } finally {
      busy.value = false
    }
    // 列表刷新失败不影响已经生效的切换：顶部至少有新会话里的租户名。
    try {
      tenants.value = await authApi.tenants()
    } catch {
      // 下拉列表保持旧值，下一次进入布局时会重试。
    }
  }

  // 展示名属于全局账号：改一次，所有租户里看到的名字都变。
  async function updateProfile(displayName: string): Promise<void> {
    const latest = await authApi.updateProfile(displayName)
    profile.value = latest
    saveProfile(latest)
    const mine = tenants.value.find((t) => t.tenant_id === latest.tenant_id)
    if (mine) mine.role = latest.role
  }

  // 改密成功后服务端已撤销全部刷新链；会话清理由调用方在跳转前完成，
  // 因为要先展示成功提示再回到登录页。
  async function changePassword(currentPassword: string, newPassword: string): Promise<void> {
    await authApi.changePassword(currentPassword, newPassword)
  }

  // 删除租户会撤销该租户下的全部会话（包括自己）。这里不自动清会话：
  // 调用方要先展示结果，再决定跳转；服务端已经保证无法续期。
  async function deleteTenant(slug: string): Promise<number> {
    const result = await authApi.deleteTenant(slug)
    return result.sessions_revoked
  }

  // 接受邀请会把当前会话切换成被邀请租户的会话：同一个浏览器里
  // 用户可能已经登录了别的租户，接受后应以新租户身份继续操作。
  async function acceptInvitation(
    token: string,
    payload: { password: string; display_name?: string },
  ): Promise<void> {
    busy.value = true
    try {
      adopt(await authApi.acceptInvitation(token, payload))
    } finally {
      busy.value = false
    }
  }

  async function logout(): Promise<void> {
    const token = storedRefreshToken()
    try {
      if (token) await authApi.logout(token)
    } catch {
      // 服务端不可达或令牌已失效，本地仍然必须清干净。
    } finally {
      clearSession()
      profile.value = null
      tenants.value = []
    }
  }

  return {
    profile,
    busy,
    tenants,
    isAuthenticated,
    isAdmin,
    isOwner,
    canWrite,
    login,
    register,
    acceptInvitation,
    refreshProfile,
    loadTenants,
    switchTenant,
    updateProfile,
    changePassword,
    deleteTenant,
    logout,
  }
})
