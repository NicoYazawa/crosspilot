// Cursor 工具测试。
//
// 覆盖：
//   - parseCursor 正常 / 非法 / 空
//   - bindToRun 跨 run 拒绝 / 空 cursor / 匹配
//   - checkGap 三种位置
//   - makeCursor 往返

import { describe, expect, it } from 'vitest';
import {
  bindToRun,
  checkGap,
  CrossRunError,
  CursorInvalidError,
  GapError,
  makeCursor,
  parseCursor,
} from './cursor';

describe('parseCursor', () => {
  it('正常解析', () => {
    expect(parseCursor('run-A:3')).toEqual({ runId: 'run-A', seq: 3 });
  });
  it('接受尾随空白', () => {
    expect(parseCursor('  run-A:3  ')).toEqual({ runId: 'run-A', seq: 3 });
  });
  it('允许 runId 含冒号（lastIndexOf）', () => {
    expect(parseCursor('run:with:colons:5')).toEqual({ runId: 'run:with:colons', seq: 5 });
  });
  it('空字符串抛 CursorInvalidError', () => {
    expect(() => parseCursor('')).toThrow(CursorInvalidError);
  });
  it('缺 seq 抛错', () => {
    expect(() => parseCursor('run-A:')).toThrow(CursorInvalidError);
  });
  it('缺 runId 抛错', () => {
    expect(() => parseCursor(':3')).toThrow(CursorInvalidError);
  });
  it('非数字 seq 抛错', () => {
    expect(() => parseCursor('run-A:abc')).toThrow(CursorInvalidError);
  });
  it('负 seq 抛错', () => {
    expect(() => parseCursor('run-A:-1')).toThrow(CursorInvalidError);
  });
  it('无冒号抛错', () => {
    expect(() => parseCursor('no-colon')).toThrow(CursorInvalidError);
  });
});

describe('makeCursor / parseCursor 往返', () => {
  it('正常往返', () => {
    const c = { runId: 'run-A', seq: 42 };
    expect(parseCursor(makeCursor(c))).toEqual(c);
  });
});

describe('bindToRun', () => {
  it('空 cursor 返回 0（首次订阅）', () => {
    expect(bindToRun(null, 'run-A')).toBe(0);
    expect(bindToRun(undefined, 'run-A')).toBe(0);
    expect(bindToRun({ runId: '', seq: 0 }, 'run-A')).toBe(0);
  });
  it('匹配时返回 seq+1', () => {
    expect(bindToRun({ runId: 'run-A', seq: 3 }, 'run-A')).toBe(4);
  });
  it('跨 run 抛 CrossRunError', () => {
    expect(() => bindToRun({ runId: 'run-A', seq: 3 }, 'run-B')).toThrow(CrossRunError);
  });
  it('CrossRunError 携带两 runId', () => {
    try {
      bindToRun({ runId: 'run-A', seq: 3 }, 'run-B');
    } catch (e) {
      const err = e as CrossRunError;
      expect(err.cursorRunId).toBe('run-A');
      expect(err.targetRunId).toBe('run-B');
    }
  });
});

describe('checkGap', () => {
  it('since=0 合法（首次订阅）', () => {
    expect(() => checkGap(10, 0)).not.toThrow();
  });
  it('since<=lastSeq+1 合法', () => {
    expect(() => checkGap(10, 5)).not.toThrow();
    expect(() => checkGap(10, 11)).not.toThrow();
  });
  it('since>lastSeq+1 抛 GapError', () => {
    expect(() => checkGap(10, 12)).toThrow(GapError);
  });
  it('GapError 携带 expected/actual', () => {
    try {
      checkGap(10, 12);
    } catch (e) {
      const err = e as GapError;
      expect(err.expected).toBe(11);
      expect(err.actual).toBe(12);
    }
  });
});
