import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [
    react(),
    {
      name: 'rename-html-and-single-bundle',
      generateBundle(_, bundle) {
        if (bundle['index.html']) {
          bundle['ui.html'] = {
            ...bundle['index.html'],
            fileName: 'ui.html',
          };
          delete bundle['index.html'];
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
