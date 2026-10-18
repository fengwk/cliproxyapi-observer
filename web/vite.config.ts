import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import fs from 'node:fs';
import path from 'node:path';

export default defineConfig({
  plugins: [
    react(),
    {
      name: 'rename-html',
      closeBundle() {
        const distDir = path.resolve(__dirname, '../internal/web/dist');
        const indexPath = path.join(distDir, 'index.html');
        const uiPath = path.join(distDir, 'ui.html');
        if (fs.existsSync(indexPath)) {
          fs.renameSync(indexPath, uiPath);
        }
      },
    },
  ],
  base: '/v0/resource/plugins/cliproxyapi-observer/',
  build: {
    outDir: '../internal/web/dist',
    emptyOutDir: true,
    cssCodeSplit: false,
    rollupOptions: {
      input: 'index.html',
      output: {
        inlineDynamicImports: true,
        entryFileNames: 'ui.js',
        chunkFileNames: 'ui.js',
        assetFileNames: (assetInfo) => {
          if (assetInfo.name && assetInfo.name.endsWith('.css')) {
            return 'ui.css';
          }
          return '[name][extname]';
        },
      },
    },
  },
});
