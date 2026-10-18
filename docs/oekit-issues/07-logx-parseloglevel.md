## 问题

`ParseLogLevel` 返回 `*Level`，解析失败返回 nil：

```go
func ParseLogLevel(s string) *Level
```

调用方必须写防御性判空，与同包 `LevelDebug/LevelInfo/...` 常量的风格不一致：

```go
level := logx.LevelInfo
if parsed := logx.ParseLogLevel(cfg.LogLevel); parsed != nil {   // 又一个 nil 分支
	level = *parsed
}
```

实际用下来这个 nil 很难被注意到——写漏判空，`LOG_LEVEL` 写错时就会得到 `*nil` 解引用 panic，堆栈指向 config 解析处，离真正的原因很远。

## 建议

提供不易用错的一种形态（breaking 的话可保留旧函数加 `Ptr` 后缀过渡）：

```go
// A：不返回 nil
func ParseLogLevel(s string) (Level, error)
// B：不返回 error
func ParseLogLevelOr(s string, def Level) Level   // 解析失败用 def
```

个人倾向 A + B 都给：A 用于需要区分「没配」与「配错了」的场景，B 用于「配错就退回默认」这种绝大多数情况。

另外建议补一句行为说明：`ParseLogLevel("")` 返回什么？按 `LOG_LEVEL` 兼容逻辑，未设置时应等价于默认级别还是报错？这类边界目前没有文档。

---
*提交自一次真实的全量重构（把一个 Prometheus exporter 整体迁到 oekit 之上）。文中数据均为实测，可按给出的命令复现。*