/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'

// `make dev`: the Go side listens on :8090, Vite proxies /api to it.
export default defineConfig({
  plugins: [vue(), tailwindcss()],
  server: {
    port: 5173,
    proxy: { '/api': { target: 'http://127.0.0.1:8090', changeOrigin: false } },
  },
  build: { outDir: 'dist', emptyOutDir: true, chunkSizeWarningLimit: 900 },
  test: { environment: 'node', include: ['src/**/*.test.ts'] },
})
