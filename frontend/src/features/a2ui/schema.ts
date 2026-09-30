// A2UI v0.9 三报文 Zod schema。
//
// 与后端 internal/agent/runevent/a2ui.go 严格对齐：
//   - ValidateCreateSurface / ValidateUpdateComponents / ValidateUpdateDataModel
//   - 字段级错误用 ZodIssue.path 暴露，便于 UI 灰卡显示具体字段
//
// 拒绝而非修复：违反契约的报文直接抛错，前端 ErrorBoundary 接住渲染灰卡。
// 这与后端 Validate* 语义一致——契约层错误不替调用方「猜正确值」。

import { z } from 'zod';
import { A2UI_CATALOG_ID, A2UI_VERSION, SHOPPING_REQUIREMENTS_PATH } from '@/types/a2ui';
import type { A2UIComponentType } from '@/types/a2ui';

const componentTypes = [
  'Column',
  'List',
  'Text',
  'TextInput',
  'ProductCard',
  'SubtotalLine',
  'ApprovalCard',
  'ClarificationForm',
  'ConfirmationCard',
] as const satisfies readonly A2UIComponentType[];

// 单组件 schema：type 字面量闭合（编译期 + 运行期双闸门）。
export const ComponentSchema = z.object({
  id: z.string().min(1, '组件 id 必填'),
  type: z.enum(componentTypes),
  path: z.string().optional(),
  props: z.record(z.string(), z.unknown()).optional(),
  surface: z.enum(['static', 'dynamic']).optional(),
});

// createSurface 必须含 /requirements 根组件（与后端 ShoppingRequirementsPath 对齐）。
export const CreateSurfaceSchema = z
  .object({
    action: z.literal('createSurface'),
    catalogId: z.literal(A2UI_CATALOG_ID),
    version: z.literal(A2UI_VERSION),
    components: z.array(ComponentSchema).min(1, 'components 不能为空'),
  })
  .refine(
    (p) => p.components.some((c) => c.path === SHOPPING_REQUIREMENTS_PATH),
    { message: `缺少 ${SHOPPING_REQUIREMENTS_PATH} 根组件` },
  );

export const UpdateComponentsSchema = z.object({
  action: z.literal('updateComponents'),
  catalogId: z.literal(A2UI_CATALOG_ID),
  components: z.array(ComponentSchema),
});

export const UpdateDataModelSchema = z.object({
  action: z.literal('updateDataModel'),
  catalogId: z.literal(A2UI_CATALOG_ID),
  value: z.object({
    path: z.string().min(1, 'value.path 必填'),
    data: z.array(z.record(z.string(), z.unknown())),
  }),
});

// 判别联合：按 action 字段自动选 schema。
export const A2UIMessageSchema = z.discriminatedUnion('action', [
  CreateSurfaceSchema,
  UpdateComponentsSchema,
  UpdateDataModelSchema,
]);

export type ValidatedComponent = z.infer<typeof ComponentSchema>;
export type ValidatedCreateSurface = z.infer<typeof CreateSurfaceSchema>;
export type ValidatedUpdateComponents = z.infer<typeof UpdateComponentsSchema>;
export type ValidatedUpdateDataModel = z.infer<typeof UpdateDataModelSchema>;
export type ValidatedA2UIMessage = z.infer<typeof A2UIMessageSchema>;

export class A2UIRenderError extends Error {
  readonly issues: z.ZodIssue[];
  constructor(message: string, issues: z.ZodIssue[]) {
    super(`${message}: ${issues.map((i) => `${i.path.join('.')}=${i.message}`).join('; ')}`);
    this.name = 'A2UIRenderError';
    this.issues = issues;
  }
}

// 校验并返回类型化报文；失败抛 A2UIRenderError。
export function validateMessage(raw: unknown): ValidatedA2UIMessage {
  const result = A2UIMessageSchema.safeParse(raw);
  if (!result.success) {
    throw new A2UIRenderError('A2UI 报文不合规', result.error.issues);
  }
  return result.data;
}
