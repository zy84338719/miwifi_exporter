# Changelog

> **1.17.1 是当前最新版本**（依赖升级 + 内部优化，指标零变化）。
> 破坏性变更只在 1.17.0，见下。

---

## ⚠️ 1.17.0 含破坏性变更

**升级前请先读完这一节。**

### 全部指标新增 `host` 标签

v1.16.0 只有一半指标带 `host`：

| 已有 `host` | 没有 `host` |
|---|---|
| `cpu_*` `memory_*` `count_*` `uptime` `wan_*` `lan_*` `wifi_sta_count` `mesh_*` | `platform` `version` `sn` `mac` `ipv4*` `ipv6` `device_*` `wifi_detail` `wifi_device_signal` `neighbor_wifi_*` |

**影响哪些查询**

| 查询写法 | 影响 |
|---|---|
| `miwifi_cpu_cores` | 不受影响 |
| `sum(miwifi_device_upload_traffic)` | 不受影响 |
| `miwifi_platform{platform="RB03"}` | 不受影响（没写 host 的匹配器照常匹配） |
| `miwifi_device_upload_traffic{ip="x",mac="y",device_name="z",is_ap="0"}` | **失配**，需补 `host="…"` |
| `sum by (host) (...)` | 新增能力：跨路由器聚合 |

**为什么必须改**：多路由器场景下，没有 `host` 的那半边会与另一台路由器的同名序列
真的撞车，Prometheus 在 Gather 阶段直接报 `duplicate metrics`。不补 `host`，
多路由器功能无法实现——这不是取舍，是前提。

**怎么排查**：升级后若某条查询返回空，先确认它是不是写全了标签集；若是，
补上 `host` 即可。指标名本身一个都没改。

---

## 1.17.1

补丁版本：依赖升级 + 内部优化，**指标集合与取值语义零变化**，升级无需改查询。

### 变更

- **升级 oekit 全家桶**：`concurrency` v0.6→v0.12、`pkg` v0.11→v0.13、
  `hertz` v0.5.10→v0.6.4、`middleware` v0.7→v0.9、`logx` v0.6→v0.7、
  `core` v0.7→v0.8、`bootstrap` v0.6.0→v0.6.2。顺带清掉 Dependabot 报的
  protobuf / x/sys 两个漏洞。
- **修复 `routers[].host` 从未被校验**：`ValidateRouterHost` 只作用于单路由器的
  `ROUTER_HOST`，而多机模式下那个字段根本不用。于是带引号/空格的 host 能一路
  通过校验并进到 Prometheus 标签里。现在逐台校验。
- **日志聚合同一原因去重**：路由器宕机时九个端点的错误文本完全一样，
  拼起来单行 1.4KB、九遍重复同一句话。现在按错误归并（实测降到 493 字节），
  逐端点详情降级到 `LOG_LEVEL=debug`。
- **新增并发压力测试**：stok 持续剧变下的 48 路并发采集、goroutine 泄漏、
  ctx 取消不阻塞三个场景。此前 `-race` 只在正常路径上跑过。

### 内部简化（无行为变化）

- `middleware` v0.8.0 起 `StackConfig.SkipPaths` 已贯通 Response，
  `server.go` 里手搓的整条中间件栈换回标准一键栈。
- `logx.ParseLogLevel` 改返回 `(Level, error)`，改用 `ParseLogLevelOr`
  并在解析失败时补一条 WARN。
- `concurrency.Flight.DoShared` 用于区分「真采集」与「复用并发结果」。
- `core.ApplyEnvOverlay` 替掉手写的 env 覆盖合并。

### 部署

新增 `docker-compose.multi.yml`：**一个进程采多台**的推荐写法，
省一半内存、日志只有一份、`/routers` 一屏看全。代价是只暴露一个端口，
Prometheus 需合成单个 target，跨路由器聚合改用 `sum by (host)`。

---

## 1.17.0

基于 [oekit](https://oekit.github.io/) 模块矩阵整体重写：日志、配置、并发控制、
HTTP 服务、生命周期全部换成 oekit 组件，并新增多路由器与固件适配能力。
版本号沿用上游 1.x 发布线（上游最后一个是 1.16.0），本 fork 从 1.17.0 继续。

> 关于为什么破坏性变更仍用小版本号：这是 fork 的版本策略选择，
> 上游并不发这个版本，因此不存在与上游发布线的冲突。若你更倾向用大版本
> 表达破坏性变更，可自行改为 `2.0.0`——只需改 `VERSION`、
> `Chart.yaml` 的 `appVersion` 与本文件的标题。

### 破坏性变更

#### 所有指标都增加了 `host` 标签

v1 只有一半指标带 `host`：

| 已有 `host` | 没有 `host` |
|---|---|
| `cpu_*` `memory_*` `count_*` `uptime` `wan_*` `lan_*` `wifi_sta_count` `mesh_*` | `platform` `version` `sn` `mac` `ipv4*` `ipv6` `device_*` `wifi_detail` `wifi_device_signal` `neighbor_wifi_*` |

**为什么必须改**：多路由器场景下，没有 `host` 的那半边会与另一台路由器的同名
序列真的撞车，Prometheus 在 Gather 阶段直接报错：

```
collected metric "miwifi_platform" {…} was collected before with
the same name and label values
```

不补 `host`，多路由器功能无法实现。这不是取舍，是前提。

**影响面**

| 查询写法 | 影响 |
|---|---|
| `miwifi_cpu_cores` | 不受影响 |
| `sum(miwifi_device_upload_traffic)` | 不受影响 |
| `miwifi_platform{platform="RB03"}` | 不受影响（没写 host 的匹配器照常匹配） |
| `miwifi_device_upload_traffic{ip="x",mac="y",device_name="z",is_ap="0"}` | **失配**，需补 `host="…"` |
| `sum by (host) (...)` | 新增能力：跨路由器聚合 |

**迁移**：只需给「写死完整标签集」的查询补上 `host`。绝大多数 PromQL 不受影响。

指标名本身一个都没改。

### 新增

- **多路由器支持**：一个 exporter 采 N 台，各台独立登录 / profile / 熔断 / 超时。
  三种配置写法：`routers` 数组、`ROUTERS` 环境变量（JSON）、旧的单台字段。
- **固件 profile 适配**（`internal/profile`）：把「随型号/固件版本而变」的部分
  （端点可用性、口令散列、登录页解析、频段映射）从采集逻辑里摘出来。
  内置 `auto` / `default` / `minimal` / `no-scan` / `no-xqnetwork`。
- **`no-scan` profile**（推荐）：摘掉会触发 WiFi 扫描的 `wifi_list`。
  真机实测该端点要 13–14s，且扫描期间路由器阻塞其余全部 LuCI 请求，
  导致另外九个端点跟着一起超时。摘掉后 9/9 端点可用、抓取从 5s 降到 1–3s。
- **`/routers` 调试端点** + 首页状态表：逐台查看 profile、启用的端点、
  被跳过的端点、超时次数、schema 是否对齐。
- **`/livez` `/readyz` `/healthz`** 探针（v1 只有 `/`）。
- **exporter 自监控指标族**（`<ns>_exporter_*`）：`router_up{host}`、
  `endpoint_up{host,endpoint}`、`endpoint_stale`、`endpoint_supported`、
  `scrape_duration_seconds`、`snapshot_age_seconds` 等。
- **poll 模式**：后台周期采集，`/metrics` 只读内存快照（微秒级返回）。
- **`ENDPOINT_TIMEOUTS` / `EXCLUDE_ENDPOINTS` / `ROUTER_PROFILE`** 逃生口。
- 配置文件支持时长字符串：`"routerTimeout": "8s"`。
- GitHub Actions：`main`/`v*` 推送时构建 amd64/arm64/arm-v7 镜像并做真实冒烟。

### 修复

| 问题 | v1 行为 |
|---|---|
| **启动日志打印明文密码** | `log.Println("密码: ", …)` —— 日志一外泄等于公开路由器后台 |
| **stok 只在启动时取一次** | 路由器一重启，exporter 永久输出垃圾数据且无人知晓 |
| **token 失效会杀掉进程** | 重登失败走 `log.Fatalf` → `os.Exit(1)` |
| **十次串行请求** | 一次 scrape = 十个 RTT 之和；现并发，约等于最慢那个 |
| **无超时** | `http.Client{}` 零值超时，路由器挂了就永久挂住 |
| **正则结果直接取下标** | 固件改版即 `index out of range` panic |
| **近半数指标 HELP 为空** | `# HELP xxx` 后空白 |
| **无健康检查 / 无优雅关停** | K8s 探针打 `/`；`log.Fatal(ListenAndServe)` |
| **零测试** | 现 12 个测试文件、覆盖率 76.8%，含 `-race` |
| **抓取超时不停止** | Prometheus 断开后仍会把十个接口跑完 |

### 行为变更（非破坏）

- **告警日志聚合为一行**。v1 十个端点各打一条 WARN，路由器宕机时一昼夜两万行；
  现在每轮一行，逐端点错误都在里面（`LOG_LEVEL=debug` 可看明细）。
- **超时不再触发端点停问**。曾实现「连续 3 次超时就停问」，真机验证发现它是错的：
  超时无法区分「这个端点坏了」与「路由器被别的端点堵住了」，
  实测 8 次抓取里误杀了 7 个健康端点。现在只认 404/501。
- **聚合类指标恒定存在**。跨台对比面板默认引用它们，取不到就发 0，
  不再因为某台没数据就整条消失（`wan_negotiation_speed` 是因此修的）。

### 依赖

`github.com/oekit/{logx, pkg, concurrency, core, bootstrap, middleware, hertz}`，
Go ≥ 1.26。
