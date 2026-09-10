import { fileURLToPath } from "node:url";

import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const appRoot = fileURLToPath(new URL("./", import.meta.url));

export default defineConfig({
  base: "./",
  envDir: appRoot,
  plugins: [react()],
  build: {
    outDir: "../web",
    emptyOutDir: true,
  },
  server: {
    port: 4179,
    host: "127.0.0.1",
    proxy: {
      "/ws": {
        target: "ws://127.0.0.1:8080",
        ws: true,
        changeOrigin: false,
      },
    },
  },
});
