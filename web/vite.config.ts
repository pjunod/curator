import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Dev: `npm run dev` serves on :5173 and proxies API calls to the Go server
// (`make dev-api`) on :7676. Prod: `npm run build` → dist/, embedded into the
// binary via go:embed.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': 'http://localhost:7676',
    },
  },
})
