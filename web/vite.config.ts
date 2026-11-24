import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vitest/config";

const root = fileURLToPath(new URL(".", import.meta.url));

export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: "/",
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: false,
    rollupOptions: {
      input: {
        main: resolve(root, "index.html"),
        prototypeA: resolve(root, "prototypes/a/index.html"),
        prototypeB: resolve(root, "prototypes/b/index.html"),
      },
    },
  },
  server: {
    port: 5173,
    strictPort: true,
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./tests-unit/setup.ts"],
    include: ["tests-unit/**/*.test.{ts,tsx}", "src/**/*.test.{ts,tsx}"],
  },
});
