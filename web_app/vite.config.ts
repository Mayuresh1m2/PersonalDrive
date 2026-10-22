import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// `pnpm dev` proxies the API to a local server (PDRIVE_URL, default http://127.0.0.1:8443).
export default defineConfig({
  plugins: [react()],
  build: { outDir: "dist", sourcemap: false, chunkSizeWarningLimit: 800 },
  server: {
    proxy: {
      "/api": { target: process.env.PDRIVE_URL ?? "https://127.0.0.1:8443", secure: false, ws: true },
    },
  },
  test: { environment: "jsdom" },
});
