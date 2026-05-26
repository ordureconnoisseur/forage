import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { viteSingleFile } from "vite-plugin-singlefile";

// Single-file build: the SPA inlines into dist/index.html. The vanilla
// entry script `forage.entry.js` lives in public/ so Vite copies it
// untouched — it runs inside Stash's main SPA via PluginApi and adds
// the nav entry point.
export default defineConfig({
  base: "./",
  plugins: [react(), viteSingleFile()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    assetsInlineLimit: 10_000_000,
  },
  server: {
    port: 5173,
  },
});
