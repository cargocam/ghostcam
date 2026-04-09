/// <reference types="vitest/config" />
import { svelte } from '@sveltejs/vite-plugin-svelte';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vite';
import path from 'path';

export default defineConfig({
  plugins: [tailwindcss(), svelte()],
  test: {
    include: ['src/**/*.test.ts'],
    exclude: ['e2e/**'],
  },
  resolve: {
    alias: {
      $lib: path.resolve('./src/lib'),
    },
  },
  build: {
    outDir: 'build',
    assetsDir: '.',
  },
  server: {
    port: 5173,
    host: true,
    proxy: {
      '/api': {
        target: process.env.VITE_API_URL ?? 'http://localhost:3000',
      },
      '/events': {
        target: process.env.VITE_API_URL ?? 'http://localhost:3000',
      },
      '/hls': {
        target: process.env.VITE_API_URL ?? 'http://localhost:3000',
      },
    },
  },
});
