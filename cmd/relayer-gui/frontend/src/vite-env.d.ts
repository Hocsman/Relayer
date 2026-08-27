/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_RELAYER_DEMO?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
