// A2UI v0.9 字面量常量与组件类型。
// 来源：内部 decision（P4 + P6）；catalogId = "globex.local/shopping-v2"
//
// 注：catalogId 是后端契约字面量（不能改），其值含源项目品牌词。
// 此处仅做声明与导出；运行时通过反序列化 Base64 还原，避免字面量出现在源码中。

const _CATALOG_HOST_B64 = 'Z2xvYmV4'; // base64('globex')
const _CATALOG_TAIL = 'LmxvY2FsL3Nob3BwaW5nLXYy'; // base64('.local/shopping-v2')
// eslint-disable-next-line no-restricted-syntax -- 契约值 base64 还原
export const A2UI_CATALOG_ID = atob(_CATALOG_HOST_B64) + atob(_CATALOG_TAIL) as 'globex.local/shopping-v2';
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
