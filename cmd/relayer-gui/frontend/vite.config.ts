import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  build: {
    target: "es2022",
    outDir: "dist",
    emptyOutDir: true,
  },
  server: {
    strictPort: true,
  },
  test: {
    environment: "node",
    include: ["src/**/*.test.{ts,tsx}"],
    // Vitest turns every CSS import into an empty string unless it is listed
    // here, ?raw included, so styles.test.ts parsed nothing and checked
    // nothing.
    css: { include: [/styles\.css/] },
  },
});
