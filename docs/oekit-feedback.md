# 给 oekit 的改进需求

> 来源：把 `helloworlde/miwifi-exporter` 整体重构到 oekit 之上（2 轮迭代、约 6700 行代码）。
> 下面每一条都来自实际编码过程，不是纸面推演。带 **验证** 的是可复现的证据。

---

## P0 · 会让使用方直接装错/用错

### 1. `versions.json` 的版本号大面积过期，且自称当天生成

**验证**（`https://oekit.github.io/versions.json`，`generatedAt: 2026-10-17T13:20:35Z`，与本次重构同一天）：

| 模块 | versions.json 声称 | 实际 `@latest` |
|---|---|---|
| logx | v0.5.0 | **v0.6.0** |
| core | v0.6.0 | **v0.7.0** |
| concurrency | v0.5.4 | **v0.6.0** |
| bootstrap | v0.5.4 | **v0.6.1** |
| middleware | v0.6.2 | **v0.7.1** |
| hertz | v0.5.10 | **v0.6.0** |
| httpx | v0.3.2 | **v0.4.1** |
| registry | v0.4.0 | **v0.4.1** |
| pkg / data / timertask / gormx / network | — | 一致 |

12 个模块里 8 个是旧的，而文件声称是当天生成的。这比「不更新」更糟：Agent 会把它当成权威快照照单钉版本。

本次重构我按 `versions.json` 的指引选了 `hertz v0.5.10`，实际已经是 v0.6.0。

**需求**：
- 排查 `matrix-tools/scripts/gen_versions.py` 的取数来源（看起来读的是某个滞后镜像，而不是 tag 列表）
- CI 里加一道校验：重新生成后与已发布内容 diff，非空则 fail，防止陈旧数据被当成新鲜数据发出去

### 2. `llms.txt` 里 middleware 的路径描述是错的

**验证**：`llms.txt` 第 23 行：

> `middleware](https://github.com/oekit/middleware): Hertz 标准中间件栈（包名 mw，路径 middleware/middleware 双层目录）`

实际情况：`middleware@v0.7.0` 的包在**模块根**，包名 `mw`，即 `github.com/oekit/middleware`，**不存在 `middleware/middleware`**。

`llms.txt` 是 AI Agent 的入口文档，这一条会让 Agent 直接写错 import。本次重构我第一版就是这么写的：

```go
middleware.RecoveryMiddleware()   // ❌ 编译不过：包名是 mw
```

**需求**：修正为「包名 `mw`，模块根包 `github.com/oekit/middleware`」。

---

## P1 · 「一键标准栈」在标准协议端点上不可用

### 3. `mw.DefaultMiddleware` 无法正确服务 `/metrics`

**验证**（`middleware@v0.7.0/stack.go:40-51`）：

```go
func DefaultMiddleware(cfg StackConfig) []app.HandlerFunc {
	stack = append(stack, hmw.Middleware()...)
	stack = append(stack,
		RecoveryMiddleware(),
		ResponseMiddleware(),                                    // ← 无任何 option
		AccessLogMiddleware(WithAccessLogSkipPaths(cfg.SkipPaths...)),  // ← 只有它拿到 SkipPaths
	)
	return stack
}
```

`StackConfig.SkipPaths` **只**喂给 AccessLog。`ResponseMiddleware` 仍然会把响应包成 `{code,data,message,trace_id}`。

后果：任何 exporter / metrics / health / OpenAPI 这类**非 JSON 业务包壳**端点，用「一键标准栈」都会被包一层——Prometheus 拿到 `{"code":0,"data":"# HELP ..."}` 直接解析失败。而 `StackConfig` 没有任何字段能把这个 skip 传给 Response。

README 里这段示例会误导读者以为配了 SkipPaths 就够了：

```go
mw.DefaultMiddleware(mw.StackConfig{SkipPaths: []string{"/metrics", "/healthz"}})
```

**本次重构因此手搓了整条栈**：

```go
stack := hmw.Middleware()
stack = append(stack,
    mw.RecoveryMiddleware(),
    mw.ResponseMiddleware(mw.WithSkipPaths(skipPaths...)),   // 必须单独传
    mw.AccessLogMiddleware(mw.WithAccessLogSkipPaths(skipPaths...)),
)
```

**需求**（任选）：
- 给 `StackConfig` 加 `ResponseSkipPaths []string`，`DefaultMiddleware` 透传给 `ResponseMiddleware`
- 或让 `SkipPaths` 同时作用于 Response 与 AccessLog（语义更简单，但会改变现有使用者行为，属 breaking）
- 至少：README 里明确标注「DefaultMiddleware 适用于业务 JSON 端点；`/metrics`、健康探针等标准协议端点需自行拼栈并给 ResponseMiddleware 传 WithSkipPaths」

### 4. `healthz` 包文档示例的签名是错的

**验证**（`bootstrap@v0.6.0/healthz/healthz.go`）：

包文档第 17-18 行：

```go
status, body := healthz.Handle(c)   // c 是 *app.RequestContext
c.JSON(status, body)
```

实际签名：

```go
func Handle(ctx context.Context) (int, string)   // 第二返回值是 string，不是结构体
```

两处都错：传参（`*app.RequestContext` 恰好实现了 `context.Context`，**能编译通过**，更隐蔽），返回值用法（`c.JSON(status, body)` 会把一个 JSON 字符串再序列化一次，得到 `"{\"status\":\"ok\"}"` 这种双重编码）。

**需求**：修正示例为

```go
h.GET("/healthz", func(ctx context.Context, c *app.RequestContext) {
    status, body := healthz.Handle(ctx)
    c.Data(status, "application/json; charset=utf-8", []byte(body))
})
```

---

## P2 · 跨仓文档矛盾与 API 体验

### 5. `hertz.NewLogger` 的禁用示例点名了一个函数，但它本身不是 hlog-backed

**验证**（`hertz@v0.5.10/log.go:43`）：

> ⚠️ 禁止传入 hlog 背端的 Logger（**如 logx.WithService(...)**）……会形成无限递归

但 `logx@v0.6.0/logger.go:177`：

```go
func WithService(name string) Logger { return Default().WithService(name) }
```

`WithService` 继承全局默认后端——默认是 slog/std 时它**根本不是 hlog-backed**，不会 panic。文档把「是否 hlog 背端」和「用哪个函数」混为一谈，按文档行事的人会误以为 `WithService` 一定不能用。

**需求**：改成按判据描述，而非点名函数：

> 禁止传入 `logx.IsHlogBacked(l) == true` 的 Logger（例如先 `hlog.SetLogger(...)` 改变了全局默认背端之后再取 `logx.WithService(...)`），否则 adapter → logx → hlog → adapter 无限递归。

### 6. `logx.ParseLogLevel` 返回 `*Level`，失败返回 nil

调用方必须写防御性判空，与同包 `LevelDebug/LevelInfo` 常量风格不一致：

```go
level := logx.LevelInfo
if parsed := logx.ParseLogLevel(cfg.LogLevel); parsed != nil {  // 又一个 nil 判断
    level = *parsed
}
```

**需求**：`ParseLogLevel(s string) (Level, error)`（breaking，可留 `ParseLogLevelPtr` 过渡）；或补一个 `ParseLogLevelOr(s string, def Level) Level`。

### 7. `singleflight.Flight.Do` 不返回 `Shared`，`DoChan` 返回

```go
func (f *Flight[T]) Do(ctx, key, fn) (T, error)
func (f *Flight[T]) DoChan(ctx, key, fn) <-chan Result[T]   // Result 有 Shared bool
```

想在日志里区分「这次是真采集还是复用了并发中的结果」——这是折叠逻辑最有价值的可观测信号——`Do` 做不到，只能放弃或改用 `DoChan`。

**需求**：`Do` 也返回 `Result[T]`，或补 `DoShared(ctx, key, fn) (T, bool, error)`。

### 8. `pkg/errors` 没有 `New`，哨兵要同时 import 标准库 `errors`

`Wrap`/`Append`/`Chain` 来自 `pkg/errors`，但造哨兵只能用标准库 `errors.New`。结果同一个文件里出现两个 `errors` 包：

```go
import (
    stderrors "errors"
    oerr "github.com/oekit/pkg/errors"
)
var ErrStokInvalid = stderrors.New("...")   // 只能这样
```

「类型同一性红线」的目的是避免 BizError 的 As/Is 判定断裂，而哨兵走标准库反而是这个原则的正确用法——但文档里没有一句话说清楚，读者会以为漏了。

**需求**：补 `errors.New`（与 Wrap/Append 同族），或在 `pkg/errors` 文档里明确写「哨兵用标准库 `errors`，BizError 才用本包，原因见类型同一性红线」。

### 9. `core/config` 的优先级说明对「无 yaml 场景」有误导

文档反复强调「yaml 显式值 > env tag > default tag」，`ApplyEnv` 也确实只填零值字段。但现实中大量项目是 **env 优先于配置文件**（12-factor，Helm/compose 都靠 env）。

按文档直译的结果是 env 无法覆盖文件里已显式写的值。本次重构为此自己搭了一层：

```go
envCfg := &Config{}
config.ApplyEnv(envCfg)      // 在空结构体上先解 env
config.OverlayMerge(cfg, envCfg)  // 再用非零值压过文件
```

（`OverlayMerge` 的语义「src 的非零值覆盖 dst」正好是这个需求，只是没人告诉你可以这么组合。）

**需求**：在 `core/config` 文档里加一节「env 优先于配置文件的写法」，给出上面这段可直接抄的示例；或直接提供 `LoadEnvOverlay[T any](dst *T) error` 之类的辅助函数。

### 10. `healthz` 是包级单例，`RegisterCheck` 追加不去重

同一进程内 `New` 两次 server（例如测试），同名 check 会被追加两次，`Handle` 每次都跑两遍。`ResetForTest` 能缓解测试侧，但生产侧没有防护。

**需求**：`RegisterCheck` 同名时覆盖而非追加（行为更符合直觉）；或提供实例化入口 `NewRegistry()` + `(*Registry).Handle(ctx)`，包级单例作为默认实例保留。

---

## 优先级建议

| # | 标题 | 类型 | 影响面 |
|---|---|---|---|
| 1 | versions.json 版本过期 | 数据 | 所有 Agent（会自动钉错版本） |
| 2 | llms.txt middleware 路径错误 | 文档 | 所有 Agent（写错 import） |
| 3 | DefaultMiddleware 不能服务 /metrics | 功能 | 所有 exporter/metrics 类项目 |
| 4 | healthz 文档示例签名错误 | 文档 | healthz 使用方 |
| 5 | NewLogger 禁用示例不准确 | 文档 | hertz 使用方 |
| 6 | ParseLogLevel 返回指针 | API | 全部使用方 |
| 7 | singleflight Do 缺 Shared | API | 需要观测折叠效果的项目 |
| 8 | pkg/errors 缺 New | API | 全部使用方 |
| 9 | env 优先于文件的写法未文档化 | 文档 | 12-factor 项目 |
| 10 | healthz 单例不可重用 | 设计 | 多 server 场景 |
