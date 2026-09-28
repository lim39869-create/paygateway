import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 构建产物直接输出到 Go embed 目录（internal/admin/static）
export default defineConfig({
  plugins: [vue()],
  build: {
    outDir: '../../internal/admin/static',
    emptyOutDir: true,
    assetsDir: 'assets',
    chunkSizeWarningLimit: 1024
  },
  server: {
    port: 5173,
    // 开发时代理到管理后台 API
    proxy: {
      '/api': 'http://localhost:8081'
    }
  }
})
