import type { Session } from './types'

// 令牌存取集中在这里，避免 api 层与 store 互相 import。
const ACCESS_KEY = 'expensio.access_token'
const REFRESH_KEY = 'expensio.refresh_token'
const PROFILE_KEY = 'expensio.profile'

export function saveSession(session: Session): void {
  localStorage.setItem(ACCESS_KEY, session.access_token)
  localStorage.setItem(REFRESH_KEY, session.refresh_token)
  localStorage.setItem(PROFILE_KEY, JSON.stringify(session.profile))
}

export function updateTokens(access: string, refresh: string): void {
  localStorage.setItem(ACCESS_KEY, access)
  localStorage.setItem(REFRESH_KEY, refresh)
}

export function accessToken(): string | null {
  return localStorage.getItem(ACCESS_KEY)
}

export function refreshToken(): string | null {
  return localStorage.getItem(REFRESH_KEY)
}

export function loadProfile(): Session['profile'] | null {
  const raw = localStorage.getItem(PROFILE_KEY)
  if (!raw) return null
  try {
    return JSON.parse(raw) as Session['profile']
  } catch {
    return null
  }
}

export function saveProfile(profile: Session['profile']): void {
  localStorage.setItem(PROFILE_KEY, JSON.stringify(profile))
}

export function clearSession(): void {
  localStorage.removeItem(ACCESS_KEY)
  localStorage.removeItem(REFRESH_KEY)
  localStorage.removeItem(PROFILE_KEY)
}
