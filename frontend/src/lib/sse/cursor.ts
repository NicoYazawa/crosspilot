// Cursor 解析与跨 run 拒绝逻辑。
//
// 与后端 internal/presentation/agui/replay.go 对齐：
//   - ParseCursor 语义同 ParseCursor："{runId}:{seq}" → Cursor
//   - bindToRun 语义同 BindToRun：cursor.RunID == runId 才合法
//   - checkGap 语义同 CheckGap：since > lastSeq+1 视为缺口
//
// 任何不合规输入都必须在协议层抛错，不允许「自动校正」——
// 跨 run 数据泄漏比断流更糟糕。

import type { Cursor } from './types';

export class CursorInvalidError extends Error {
  constructor(raw: string) {
    super(`cursor 格式不合法：${raw}`);
    this.name = 'CursorInvalidError';
  }
}

export class CrossRunError extends Error {
  readonly cursorRunId: string;
  readonly targetRunId: string;
  constructor(cursorRunId: string, targetRunId: string) {
    super(`cursor.runId=${cursorRunId} 与目标 ${targetRunId} 不匹配`);
    this.name = 'CrossRunError';
    this.cursorRunId = cursorRunId;
    this.targetRunId = targetRunId;
  }
}

export class GapError extends Error {
  readonly expected: number;
  readonly actual: number;
  constructor(expected: number, actual: number) {
    super(`重连点 ${actual} 与服务端最后序号 ${expected} 不连续`);
    this.name = 'GapError';
    this.expected = expected;
    this.actual = actual;
  }
}

// 把 cursor 序列化成 "{runId}:{seq}" 字符串。
//
// 用字符串拼接而非模板：cursor 必须稳定可读、便于调试时直接 grep。
export function makeCursor(c: Cursor): string {
  return `${c.runId}:${c.seq}`;
}

// 解析 "{runId}:{seq}"。允许尾随空白；不允许空 runId / 负 seq / 非数字 seq。
export function parseCursor(raw: string): Cursor {
  const trimmed = raw.trim();
  if (trimmed === '') throw new CursorInvalidError(raw);
  const idx = trimmed.lastIndexOf(':');
  if (idx <= 0 || idx === trimmed.length - 1) {
    throw new CursorInvalidError(raw);
  }
  const runId = trimmed.slice(0, idx);
  const seqStr = trimmed.slice(idx + 1);
  const seq = Number.parseInt(seqStr, 10);
  if (!Number.isFinite(seq) || seq < 0 || String(seq) !== seqStr) {
    throw new CursorInvalidError(raw);
  }
  return { runId, seq };
}

// 校验 cursor 与目标 run 是否匹配，返回「重连起点 seq=cursor.seq+1」。
//
// cursor 为空时返回 0（首次订阅，从头开始）。
// 跨 run 时抛 CrossRunError，不允许悄悄校正。
export function bindToRun(cursor: Cursor | null | undefined, expectedRunId: string): number {
  if (cursor === null || cursor === undefined || cursor.runId === '') {
    return 0;
  }
  if (cursor.runId !== expectedRunId) {
    throw new CrossRunError(cursor.runId, expectedRunId);
  }
  return cursor.seq + 1;
}

// 校验客户端请求的起点是否与服务端 lastSeq 连续。
//
// since=0 → 首次订阅（合法）
// since<=lastSeq+1 → 至多落后一格（合法）
// since>lastSeq+1 → 缺口（抛 GapError）
export function checkGap(lastSeq: number, since: number): void {
  if (since === 0) return;
  if (since <= lastSeq + 1) return;
  throw new GapError(lastSeq + 1, since);
}
