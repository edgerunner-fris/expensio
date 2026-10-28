import axios, {
  AxiosError,
  type AxiosRequestConfig,
  type InternalAxiosRequestConfig,
} from 'axios'
import { accessToken, clearSession, refreshToken, saveSession } from './session'
import type { Session } from './types'

export interface ApiEnvelope<T> {
  code: string
  message?: string
  request_id?: string
  data: T
  details?: Record<string, unknown>
}

export interface Page<T> {
  items: T[]
  total: number
  page: number
  page_size: number
}

const http = axios.create({ baseURL: '/api/v1', timeout: 15000 })

http.interceptors.request.use((config) => {
  const token = accessToken()
  if (token) config.headers.Authorization = `Bearer ${token}`
  return config
})

// 同一时刻只允许一个刷新请求：并发 401 各自去刷新会触发后端的重用检测，
// 把整族令牌撤销掉。
let refreshing: Promise<string> | null = null

async function refreshAccess(): Promise<string> {
  if (!refreshing) {
    refreshing = (async () => {
      const token = refreshToken()
      if (!token) throw new Error('缺少刷新令牌')
      const resp = await axios.post<ApiEnvelope<Session>>('/api/v1/auth/refresh', {
        refresh_token: token,
      })
      saveSession(resp.data.data)
      return resp.data.data.access_token
    })().finally(() => {
      refreshing = null
    })
  }
  return refreshing
}

http.interceptors.response.use(
  (resp) => resp,
  async (error: AxiosError) => {
    const config = error.config as (InternalAxiosRequestConfig & { _retried?: boolean }) | undefined
    const isAuthCall = config?.url?.includes('/auth/') ?? false
    if (error.response?.status === 401 && config && !config._retried && !isAuthCall) {
      config._retried = true
      try {
        const token = await refreshAccess()
        config.headers.Authorization = `Bearer ${token}`
        return http(config)
      } catch {
        clearSession()
        if (window.location.pathname !== '/login') window.location.assign('/login')
      }
    }
    return Promise.reject(error)
  },
)

export function apiMessage(err: unknown): string {
  if (axios.isAxiosError<ApiEnvelope<unknown>>(err)) {
    return err.response?.data?.message ?? err.message
  }
  return err instanceof Error ? err.message : '请求失败'
}

export function errorCode(err: unknown): string {
  if (axios.isAxiosError<ApiEnvelope<unknown>>(err)) {
    return err.response?.data?.code ?? ''
  }
  return ''
}

async function data<T>(request: Promise<{ data: ApiEnvelope<T> }>): Promise<T> {
  const resp = await request
  return resp.data.data
}

// download 走同一个 axios 实例，因此 401 刷新与错误处理与普通请求一致；
// 响应体是文件而不是业务信封，调用方自行决定文件名。
//
// 超时单独放宽：导出最多 5 万行，不能用 15 秒的接口超时。
// 失败时后端返回的仍是 JSON 信封，但 responseType=blob 会把它包成 Blob，
// 这里解出来再抛，调用方用 apiMessage 就能显示服务端的具体原因。
export async function download(
  url: string,
  params?: Record<string, unknown>,
  timeout = 60000,
): Promise<Blob> {
  try {
    const resp = await http.get(url, { params, responseType: 'blob', timeout })
    return resp.data as Blob
  } catch (err) {
    if (axios.isAxiosError(err) && err.response?.data instanceof Blob) {
      const message = await blobMessage(err.response.data)
      if (message) throw new Error(message)
    }
    throw err
  }
}

async function blobMessage(blob: Blob): Promise<string> {
  try {
    const parsed = JSON.parse(await blob.text()) as ApiEnvelope<unknown>
    return parsed.message ?? ''
  } catch {
    return ''
  }
}

export const api = {
  get<T>(url: string, config?: AxiosRequestConfig): Promise<T> {
    return data<T>(http.get<ApiEnvelope<T>>(url, config))
  },
  post<T>(url: string, body?: unknown): Promise<T> {
    return data<T>(http.post<ApiEnvelope<T>>(url, body))
  },
  put<T>(url: string, body?: unknown): Promise<T> {
    return data<T>(http.put<ApiEnvelope<T>>(url, body))
  },
  patch<T>(url: string, body?: unknown): Promise<T> {
    return data<T>(http.patch<ApiEnvelope<T>>(url, body))
  },
  // body 可选：删除租户要求回写 slug 作为二次确认，而 axios 的
  // DELETE 请求体必须放在 config.data 里。
  delete<T>(url: string, body?: unknown): Promise<T> {
    return data<T>(http.delete<ApiEnvelope<T>>(url, body === undefined ? undefined : { data: body }))
  },
}
