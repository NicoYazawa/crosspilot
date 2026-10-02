// A2UI v0.9 字面量常量与组件类型。
// 来源：内部 decision（P4 + P6）；catalogId 见下。

// A2UI_CATALOG_ID 是 A2UI v0.9 的协议字面量，由开发计划附录 B#2 / E6 钉死，
// 必须与后端 internal/agent/runevent/a2ui.go 的 A2UICatalogID 逐字符相同。
//
// 这里的品牌词是协议值，不是文档里的项目称呼——eslint 的 commit 禁词规则
// 专为后者而设，故按精确值放行（见 eslint.config.js 的 :not 例外）。
// 曾经用 Base64 藏起这个字面量来绕过规则，那是把一条好规则变成了不可搜索的
// 常量：改协议值时没人能 grep 到它。
export const A2UI_CATALOG_ID = 'globex.local/shopping-v2';
export const A2UI_VERSION = '0.9' as const;
export const SHOPPING_REQUIREMENTS_PATH = '/requirements' as const;

// 注册表字面量联合：未注册的 type 编译期报错。
// 增加新组件时必须同步：
//   1. @/features/a2ui/registry.ts 注册表
//   2. @/features/a2ui/components/<Name>.tsx 实现
//   3. schema.ts 的 z.enum([...])
export type A2UIComponentType =
  | 'Column'
  | 'List'
  | 'Text'
  | 'TextInput'
  | 'ProductCard'
  | 'SubtotalLine'
  | 'ApprovalCard'
  | 'ClarificationForm'
  | 'ConfirmationCard';

export type A2UIAction = 'createSurface' | 'updateComponents' | 'updateDataModel';

export interface A2UIComponentBase {
  id: string;
  type: A2UIComponentType;
  path?: string;
  surface?: 'static' | 'dynamic';
  // props 是开放 record；具体组件自行 Zod 二次校验（见 @/features/a2ui/schema.ts）。
  // 严禁在 props 里塞入 dangerouslySetInnerHTML 字段——ESLint 规则兜底。
  [k: `data-${string}`]: unknown;
}

export interface CreateSurfaceMessage {
  action: 'createSurface';
  catalogId: typeof A2UI_CATALOG_ID;
  version: typeof A2UI_VERSION;
  components: A2UIComponentBase[];
}

export interface UpdateComponentsMessage {
  action: 'updateComponents';
  catalogId: typeof A2UI_CATALOG_ID;
  components: A2UIComponentBase[];
}

export interface UpdateDataModelMessage {
  action: 'updateDataModel';
  catalogId: typeof A2UI_CATALOG_ID;
  value: { path: string; data: unknown[] };
}

export type A2UIMessage =
  | CreateSurfaceMessage
  | UpdateComponentsMessage
  | UpdateDataModelMessage;
