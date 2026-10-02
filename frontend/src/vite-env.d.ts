/// <reference types="vite/client" />

// 构建期注入的环境变量。声明在这里而不是散落在各文件里，是为了让
// 「这个前端依赖哪些部署参数」有一个可查的清单。
interface ImportMetaEnv {
  // 后端签发的 JWT。留空表示本部署未启用鉴权（后端补 demo 身份）。
  readonly VITE_AUTH_TOKEN?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
