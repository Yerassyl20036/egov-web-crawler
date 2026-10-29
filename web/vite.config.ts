import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In development the Go API runs on :8080; Vite proxies /api to it.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": { target: "http://localhost:8080", changeOrigin: true },
    },
  },
});
