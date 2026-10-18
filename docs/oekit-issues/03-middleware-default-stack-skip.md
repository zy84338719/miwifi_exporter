## 背景

`#9` 落地后 `ResponseMiddleware(WithSkipPaths(...))` 已经可用，`DefaultMiddleware` 却仍然**没有任何途径**把这个配置传进去——它内部是无参调用。

`middleware@v0.7.0/stack.go:40-51`：

```go
func DefaultMiddleware(cfg StackConfig) []app.HandlerFunc {
	stack := hmw.Middleware()
	stack = append(stack,
		RecoveryMiddleware(),
		ResponseMiddleware(),                                          // ← 无 option
		AccessLogMiddleware(WithAccessLogSkipPaths(cfg.SkipPaths...)),  // ← 只有它拿到 cfg
	)
	return stack
}
```

`StackConfig` 只有 `SkipPaths []string` 一个字段，注释也写明是「不需要记录日志的路径」——即**只作用于 AccessLog**。

## 后果

`/metrics`、健康探针、OpenAPI/JSON-RPC 这类**不能被业务信封包裹**的端点，用「一键标准栈」都会被包成 `{"code":0,"data":...}`。Prometheus 拿到这个直接解析失败，且失败信息相当含糊。

而 README 里这段示例会让人以为配了 SkipPaths 就够了：

```go
for _, m := range mw.DefaultMiddleware(mw.StackConfig{
	SkipPaths: []string{"/metrics", "/healthz"},
}) {
	h.Use(m)
}
```

## 本次的绕行写法

只能手搓整条栈（这是实际落到生产的代码）：

```go
skipPaths := []string{cfg.MetricsPath, "/healthz", "/livez", "/readyz", "/"}
stack := hmw.Middleware()
stack = append(stack,
	mw.RecoveryMiddleware(),
	mw.ResponseMiddleware(mw.WithSkipPaths(skipPaths...)),   // 必须单独传
	mw.AccessLogMiddleware(mw.WithAccessLogSkipPaths(skipPaths...)),
)
for _, m := range stack {
	h.Use(m)
}
```

「一键标准栈」在有标准协议端点的服务里反而成了不能用的东西，这个反差值得消掉。

## 建议

任选其一：

- **A（推荐，无破坏）**：给 `StackConfig` 加 `ResponseSkipPaths []string`，`DefaultMiddleware` 透传给 `ResponseMiddleware`。老代码行为不变。
- **B（语义更统一，属 breaking）**：让现有 `SkipPaths` 同时作用于 Response 与 AccessLog。需要在 CHANGELOG 标注。
- **C（仅文档）**：README 明确写「`DefaultMiddleware` 适用于业务 JSON 端点；`/metrics`、探针等标准协议端点需自行拼栈并给 `ResponseMiddleware` 传 `WithSkipPaths`」。

建议 A + C 同时做：能力补上，且把坑写进文档。

---
*提交自一次真实的全量重构（把一个 Prometheus exporter 整体迁到 oekit 之上）。文中数据均为实测，可按给出的命令复现。*