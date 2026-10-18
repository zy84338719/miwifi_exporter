## 背景

#10 落地后 `Flight` 有了 `DoChan`，但两个入口返回的东西不对称：

```go
func (f *Flight[T]) Do(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, error)
func (f *Flight[T]) DoChan(ctx context.Context, key string, fn func(context.Context) (T, error)) <-chan Result[T]
// Result[T] = { Val T; Err error; Shared bool }
```

`DoChan` 的 `Result.Shared` 正好回答一个对 singleflight 来说**最有价值**的问题：这次调用是真正执行了 `fn`，还是复用了别人正在进行中的结果。

## 缺口

这个信息现在只在 `DoChan` 上有，而绝大多数调用方用的是更简单的 `Do`——想埋点就得为了拿一个 bool 改成异步写法，把 `Do` 换成 `DoChan` + 手动 goroutine 等待，本该直白的代码被拧成了一团。

## 本次

折叠并发 scrape 时，很想在日志/指标里区分「本轮真采集」和「复用了并发中的那一轮」，用 `Do` 做不到，只能放弃这条观测。

## 建议

```go
// 二选一
func (f *Flight[T]) Do(ctx context.Context, key string, fn ...) Result[T]
func (f *Flight[T]) DoShared(ctx context.Context, key string, fn ...) (T, bool, error)
```

倾向后者：`Do` 保持 `(T, error)` 不动（不破坏现有调用点），新增 `DoShared`。若同意 `Do` 返回 `Result[T]`（Go 里也常见、且 `Do` 才是不对称的那一半），则是 breaking。

另外一个顺手的补充：`Result` 里能带上 `Waiters int`，排障时能直接看出「这一轮折叠掉了几个调用方」。

---
*提交自一次真实的全量重构（把一个 Prometheus exporter 整体迁到 oekit 之上）。文中数据均为实测，可按给出的命令复现。*