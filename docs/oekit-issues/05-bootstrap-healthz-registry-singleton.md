## 问题

`healthz` 用包级 singleton 存 check 注册表，`RegisterCheck` 是**追加且不去重**：

```go
var global = &registry{checks: map[string]checkEntry{}}

func RegisterCheck(name string, check Check) {
	global.mu.Lock()
	defer global.mu.Unlock()
	global.checks[name] = checkEntry{check: check}   // 同名会覆盖 map 项
	// 但 entries 是列表，见下
}
```

实际 `Handle` 里先把 `global.checks` 拷进 `entries` 列表再并发跑，因此**同名注册两次会跑两遍**。在同一进程里 `New` 两次 server（单元测试里很常见、某些多监听端口的服务也是）就会撞上。

`ResetForTest()` 只解决了测试侧，生产侧没有对应保护。

## 本次踩到的

单测里每个 case 都 `newTestServer(...)`，`/healthz` 的 check 数量逐个 case 累加。虽然没造成断言失败，但让「readyz 反映了哪些检查项」这个断言变得脆弱——后来只能靠 `ResetForTest` 在 setup 里手动兜。

## 建议

1. `RegisterCheck` 同名时**覆盖**并在日志里 `Warn`（覆盖比追加更符合直觉：`register` 语义就是「设置这个 check」）；
2. 或提供实例化入口，包级单例降级为默认实例：
   ```go
   type Registry struct{ ... }
   func NewRegistry() *Registry
   func (r *Registry) Register(name string, check Check)
   func (r *Registry) Handle(ctx context.Context) (int, string)
   ```
3. `Handle` 增加一个带快照语义的变体（如 `HandleSnapshot`）方便调用方/测试观察当前注册了哪些 check。

倾向 1 + 2：1 解决绝大多数场景，2 给需要多实例的场景留出口。

---
*提交自一次真实的全量重构（把一个 Prometheus exporter 整体迁到 oekit 之上）。文中数据均为实测，可按给出的命令复现。*