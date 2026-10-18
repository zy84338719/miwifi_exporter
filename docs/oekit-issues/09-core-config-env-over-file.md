## 背景

`core/config` 反复强调优先级是「**yaml 显式值 > env tag > default tag**」，`ApplyEnv` 的实现也确实只填零值字段：

```go
// ApplyEnv 依据 env tag 把环境变量写入字段：env 已设置且字段当前为零值时才填，
// 已有值（yaml 显式设置）不覆盖。
```

这个语义对 yaml 是对的。但现实中大量项目要的是**反过来的**——env 优先于配置文件（12-factor；Helm / docker-compose 基本都靠 env 传配置）。按文档直译的结果就是 env 永远盖不住文件里显式写的值。

## 本次的绕行

只能自己搭一层：

```go
envCfg := &Config{}
config.ApplyEnv(envCfg)             // 在空结构体上先解 env
config.OverlayMerge(cfg, envCfg)     // 再用非零值压过文件值
config.FillDefaults(cfg)
config.ValidateTags(cfg)
```

`OverlayMerge` 的语义（src 的非零值覆盖 dst）正好就是这个需求，但没有任何地方说明可以这么组合，等于每个 12-factor 项目都要自己发现一次。

## 建议

1. **（必须）文档**：`core/config` 增加一节「env 优先于配置文件的写法」，直接给出上面这段可抄的示例，并说明为什么不能对已填充的结构体直接 `ApplyEnv`。
2. **（可选）API**：把这个组合封成一个函数，避免每个项目重写：
   ```go
   // ApplyEnvOverlay 把环境变量解析到独立的 overlay 并覆盖 dst（src 非零值优先）。
   // 用于「env 优先于配置文件」的场景；与 ApplyEnv（只填零值）语义相反。
   func ApplyEnvOverlay(dst any) error
   ```
3. 顺带在 `LoadBootstrap` 的文档里点一句：它的接线顺序是 yaml → env → default，若消费方要 env 优先请勿直接复用。

个人感觉 1 就够解决问题，2 是锦上添花——先补文档能立刻止血。

---
*提交自一次真实的全量重构（把一个 Prometheus exporter 整体迁到 oekit 之上）。文中数据均为实测，可按给出的命令复现。*