import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发期把 /api 代理到网关：浏览器只看到 5173，不需要处理跨域，
// 生产由 ingress / 静态托管把同一路径转给网关。
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@': '/src' },
  },
  server: {
    host: '127.0.0.1',
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
    },
  },
})
