/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// Dev mode (`vite dev`) proxies /api and the WS endpoint to a running
// `smartbuster serve` instance so the UI can be iterated on with HMR without
// rebuilding the embedded bundle. Point VITE_DAEMON_TARGET at that instance's
// origin (e.g. `smartbuster serve --port 8899`, then
// VITE_DAEMON_TARGET=http://127.0.0.1:8899 npm run dev). Production is always
// the embedded static bundle served directly by the daemon (see README).
const daemonTarget = process.env.VITE_DAEMON_TARGET ?? 'http://127.0.0.1:8899'

export default defineConfig({
  plugins: [svelte()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': {
        target: daemonTarget,
        ws: true,
        changeOrigin: false,
        // The daemon's Origin check (spec §5) validates against its own
        // origin (e.g. http://127.0.0.1:8899) — but a browser talking to
        // the Vite dev server sends Origin: http://localhost:5173, which
        // will never match. Dev mode is a proxy sitting in front of the
        // real check, so rewrite Origin to what the daemon expects rather
        // than weakening the daemon's own validation.
        configure(proxy) {
          proxy.on('proxyReq', (proxyReq) => {
            proxyReq.setHeader('Origin', daemonTarget)
          })
          // node-http-proxy's ws:true fires a *separate* event for the
          // upgrade request (proxyReqWs, not proxyReq) — without this, the
          // REST Origin rewrite above silently never applies to the WS
          // handshake, which fails Security.checkOrigin and closes with
          // code 1006 before a single frame is exchanged.
          proxy.on('proxyReqWs', (proxyReq) => {
            proxyReq.setHeader('Origin', daemonTarget)
          })
        },
      },
    },
  },
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
  },
})
