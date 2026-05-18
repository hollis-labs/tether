import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// The Go binary serves this SPA under base_path (see internal/webui).
// `build.outDir` points at the Go embed directory so `npm run build`
// drops the bundle exactly where `//go:embed all:dist` expects it.
export default defineConfig({
  base: '/operations/',
  plugins: [react(), tailwindcss()],
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
  },
  server: {
    // `make ui-dev` proxies same-origin /api calls to `make run` on :8947.
    proxy: {
      '/api': 'http://localhost:8947',
    },
  },
})
