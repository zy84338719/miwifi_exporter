## 问题

`NewLogger` 的注释把「是否 hlog 背端」和一个具体函数名绑定了，但那个函数是否 hlog-backed 取决于全局状态。

`hertz@v0.5.10/log.go:41-46`：

```
⚠️ 禁止传入 hlog 背端的 Logger（如 logx.WithService(...)），否则会形成
adapter → logx → hlog → adapter 的无限递归
```

而 `logx@v0.6.0/logger.go:177`：

```go
func WithService(name string) Logger { return Default().WithService(name) }
```

`WithService` 只是 `Default()` 的派生——默认后端是 slog/std 时它**根本不是 hlog-backed**，不会 panic。只有当某处先 `hlog.SetLogger(...)` 改了全局默认背端，之后再取 `WithService` 才会触发。

## 影响

- 按文档字面理解的人，会以为 `WithService` 一定不能用，于是去写 `NewSlogLogger(...)` 造第二个后端——为了绕开一个自己臆想出来的问题
- 真正危险的那条路径（先改全局默认背端，再 `Default()`/`WithService()`）反而没被点名

## 建议

改成按**判据**而非函数名描述：

> ⚠️ 禁止传入 `logx.IsHlogBacked(l) == true` 的 Logger，否则会形成 adapter → logx → hlog → adapter 无限递归（栈溢出 + 进程挂死）。
> 常见触发路径：先 `hlog.SetLogger(...)` 改变了全局默认背端，随后用 `logx.Default()` / `logx.WithService(...)` 取到 hlog-backed 实例再传进来。
> 安全做法：`logx.IsHlogBacked(l)` 为 false 即可，或直接传 `logx.NewStdLogger(...)` / `logx.NewSlogLogger(...)`。

顺带一提，`NewLogger` 现在是 **panic** 兜底。考虑到这属于「初始化顺序配错」，panic 本身没问题（fail-fast 正确），但可以在 panic message 里带上 `logx.IsHlogBacked(l)` 的实测值与当前 `logx.Default()` 的后端类型，省掉一次猜测。

---
*提交自一次真实的全量重构（把一个 Prometheus exporter 整体迁到 oekit 之上）。文中数据均为实测，可按给出的命令复现。*