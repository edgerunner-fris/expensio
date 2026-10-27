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
import type { Profile, Session } from '@/api/types'

export const useAuthStore = defineStore('auth', () => {
  const profile = ref<Profile | null>(loadProfile())
  const busy = ref(false)

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
    }
  }

  return {
    profile,
    busy,
    isAuthenticated,
    isAdmin,
    isOwner,
    canWrite,
    login,
    register,
    acceptInvitation,
    refreshProfile,
    logout,
  }
})
