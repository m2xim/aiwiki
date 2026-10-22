import { defineConfig } from 'vite';

// AI Wiki Kit for artifacts: one self-contained IIFE (global `AIWiki`) plus one stylesheet.
// Runs after the app build, so it must not empty dist.
export default defineConfig({
  publicDir: false,
  define: { 'process.env.NODE_ENV': '"production"' },
  build: {
    outDir: 'dist/artifact-runtime',
    emptyOutDir: false,
    target: 'es2020',
    cssCodeSplit: false,
    lib: { entry: 'artifact-kit/src/index.ts', name: 'AIWiki', formats: ['iife'], fileName: () => 'aiwiki-kit.js', cssFileName: 'aiwiki-kit' },
    rollupOptions: { output: { extend: false } },
  },
});
