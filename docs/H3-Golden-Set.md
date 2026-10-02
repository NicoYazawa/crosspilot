# H3 — Golden Set 通过率 100%

## 目标
同一 query 与源项目 `embedding_only` 策略返回同一组 `product_id`（C1），金额逐分一致（C3）。

## 源项目测试范围
- 源项目：globex-agent（Python 版，GitHub）
- 待翻译测试：981 个 Python 测试函数 → Go 表驱动测试
- 测试文件位置：`/path/to/globex-agent/tests/`（需用户提供本地路径或 clone）

## 当前状态
网络不通，无法访问源项目。需用户提供：
1. 源项目本地路径，或
2. 将 globex-agent 克隆到本地

## 翻译规则
- Python `unittest.TestCase.assertEqual` → Go `if got != want { t.Errorf(...) }`
- Python `assertAlmostEqual` → Go 小数精度比较（允许 ±0.01）
- Python `assertRaises` → Go `func() { defer func(){ if r:=recover(); r!=nil{} }() }()`
- Python `parametrize` → Go 表驱动测试
- Python `fixture` → Go `setup` 函数

## 验收方式
```bash
# 翻译后运行
go test ./tests/golden/... -count=1

# 100% 通过；未通过项需登记差异
```

## 待翻译包（按优先级）
1. `domain/catalog` — 金额计算（最高优先级）
2. `domain/shipping` — 关税计算
3. `domain/trade` — 下单流程
4. `application/catalogsearch` — 检索召回
5. 其余包

## 框架占位
在 `tests/golden/` 目录下创建占位文件，待源项目就绪后填充。
