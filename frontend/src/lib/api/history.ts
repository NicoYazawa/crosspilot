// Run ID 历史缓存（IndexedDB）。
//
// 后端 P6 不暴露 GET /agui/runs（plan §留作 P7 补）。
// 前端在 SubmitRequest 成功后把 runId 写进 IndexedDB，HistoryView 读出展示。
//
// 容量：最近 20 条，超出按 lastEventAt 淘汰。
// 不在本文件范围：与后端同步 / 多设备同步（P7+）。

const DB_NAME = 'crosspilot';
const STORE = 'runHistory';
const CAP = 20;

export interface RunHistoryItem {
  runId: string;
  lastEventAt: number; // epoch ms
  query?: string;
}

function openDb(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, 1);
    req.onupgradeneeded = () => {
      const db = req.result;
      if (!db.objectStoreNames.contains(STORE)) {
        db.createObjectStore(STORE, { keyPath: 'runId' });
      }
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

export async function recordRun(item: RunHistoryItem): Promise<void> {
  const db = await openDb();
  await new Promise<void>((resolve, reject) => {
    const tx = db.transaction(STORE, 'readwrite');
    tx.objectStore(STORE).put(item);
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
  });
}

export async function listRuns(): Promise<RunHistoryItem[]> {
  const db = await openDb();
  return new Promise((resolve, reject) => {
    const tx = db.transaction(STORE, 'readonly');
    const req = tx.objectStore(STORE).getAll();
    req.onsuccess = () => {
      const all = (req.result as RunHistoryItem[]) ?? [];
      all.sort((a, b) => b.lastEventAt - a.lastEventAt);
      // 淘汰：超出 CAP 的删掉。
      if (all.length > CAP) {
        const drop = all.slice(CAP);
        const tx2 = db.transaction(STORE, 'readwrite');
        for (const it of drop) tx2.objectStore(STORE).delete(it.runId);
      }
      resolve(all.slice(0, CAP));
    };
    req.onerror = () => reject(req.error);
  });
}
