import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  base: './',
  // This is a generated public-only cache for go:embed, never the runtime data directory.
  // Vite also copies public/legal (including the original MiSans license) here.
  build: { outDir: '../internal/webui/dist', emptyOutDir: true, target: 'es2022' },
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': { target: 'http://localhost:8787', changeOrigin: false },
      '/dav': { target: 'http://localhost:8787', changeOrigin: false },
    },
  },
})
