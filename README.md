# miwifi-exporter

小米路由器（MiWiFi）的 Prometheus exporter。

![exporter](screenshot-1.png)

![grafana](screenshot-2.png)

> **v2 说明**：本版本基于 [oekit](https://oekit.github.io/) 模块矩阵完成重构
> （logx / pkg / concurrency / core / bootstrap / middleware / hertz）。
> **所有既有指标名、标签名、标签集合完全不变**，升级后 Grafana 面板与告警规则
> 无需任何修改。详见 [重构说明](#v2-重构说明)。

> **从 v1 升级请先看 [CHANGELOG](CHANGELOG.md)**：有一个破坏性变更（所有指标
> 增加 `host` 标签），绝大多数 PromQL 不受影响，受影响的只有写死完整标签集的
> 精确匹配式。

## 快速开始

```bash
docker compose build
docker compose up -d
curl http://localhost:9001/metrics
```

本地运行：

```bash
export ROUTER_IP=192.168.31.1
export ROUTER_PASSWORD=你的路由器密码
make run
```

## 配置

优先级：**命令行 flag > 环境变量 > config.json > 内置默认值**。

> 环境变量压过配置文件是刻意保留的旧行为：已经用 Helm / compose 通过环境变量
> 部署的实例不会因为某个 config.json 而行为突变。

### 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `ROUTER_IP` | **必填** | 路由器地址，支持 `192.168.31.1` / `192.168.31.1:8080` / `http://…` |
| `ROUTER_PASSWORD` | **必填** | 路由器后台密码（不会出现在日志里） |
| `ROUTER_HOST` | `miwifi` | `host` 标签的取值 |
| `ROUTER_PORT` | `9001` | exporter 自身监听端口 |
| `ROUTER_TIMEOUT` | `5s` | 单次 HTTP 请求超时 |
| `METRICS_PATH` | `/metrics` | 指标暴露路径 |
| `METRICS_NAMESPACE` | `miwifi` | 指标名前缀 |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `SHUTDOWN_TIMEOUT` | `10s` | 优雅关停预算 |
| `ROUTERS` | 空 | **多台**路由器的 JSON 数组（优先于单台字段），见「多路由器支持」 |
| `ROUTER_PROFILE` | `auto` | 固件适配方案：`auto`/`default`/`minimal`/`no-scan`/`no-xqnetwork`，见 [internal/profile](internal/profile/README.md) |
| `EXCLUDE_ENDPOINTS` | 空 | 逗号分隔，无条件跳过的端点（如 `wifi_list`） |
| `ENDPOINT_TIMEOUTS` | 空 | 按端点放宽超时，如 `wifi_list=20s` |
| `ENDPOINT_TIMEOUT_THRESHOLD` | `3` | 连续超时的告警阈值（仅观测，不停问） |
| `COLLECT_MODE` | `pull` | `pull` 或 `poll`，见下 |
| `COLLECT_INTERVAL` | `30s` | `poll` 模式的采集间隔 |
| `SCRAPE_TIMEOUT` | `15s` | `pull` 模式下单次采集的总上限 |

### 命令行 flag

| flag | 说明 |
|---|---|
| `-config` | 配置文件路径，默认 `config.json`（不存在不算错误） |
| `-web.telemetry-path` | 同 `METRICS_PATH` |
| `-metric.namespace` | 同 `METRICS_NAMESPACE` |
| `-collect-mode` | 同 `COLLECT_MODE` |
| `-version` | 打印版本后退出 |
| `-validate-config` | 只校验配置，不启动服务（适合放进 CI / 容器启动前检查） |

## 真机行为（重要）

以下结论来自对两台真实小米路由器���实测（`192.168.31.1` 与 `10.10.30.248`），
**假路由器测不出来**——它们每个接口都是瞬间返回的。

### `wifi_list` 会阻塞整台路由器

`/api/xqnetwork/wifi_list` 会触发一次 WiFi 扫描，实测需要 **13–14 秒**；
更关键的是，**扫描期间路由器会阻塞其余全部 LuCI 请求**。

后果不是「这个指标慢」，而是**其它九个本来 0.2 秒就返回的端点跟着一起超时**。
实测一轮抓取：开着它 9 个端点全挂，摘掉它 9 个端点全通。

| 配置 | 抓取耗时 | 端点可用 |
|---|---|---|
| 默认（含 wifi_list） | 5.0s（被超时钉死） | 1/10 |
| `ROUTER_PROFILE=no-scan` | **1–3s** | **9/9** |

所以推荐：

```bash
ROUTER_PROFILE=no-scan ./miwifi-exporter
```

`no-scan` 只是摘掉 `wifi_list`，其余九个端点一个不少。默认 profile 仍然保留
它（老版本升级后 `neighbor_wifi_*` 指标不该凭空消失），代价是上面那一行。

真要那个指标，就得接受慢抓取，并显式打开：

```bash
ROUTER_PROFILE=default ENDPOINT_TIMEOUTS=wifi_list=20s ./miwifi-exporter
```

并且 Prometheus 的 `scrape_timeout` 要 ≥ 16s。

### 一个反面教训

修复过程中我一度加了「连续超时 3 次就停问该端点」的启发式。**它是错的**：
超时无法区分「这个端点坏了」和「路由器被别的端点堵住了」。实测 8 次抓取里
它把 **7 个健康端点连带误杀**（含确定可用的 `status` / `lan_info` / `device_list`）。

现在超时**只作为观测信号**，停问只认 HTTP 404/501 这种确定性信号。
这个教训写进了 `internal/profile/README.md`，并有测试守着。

### 其它实测数据

| | 192.168.31.1 | 10.10.30.248 |
|---|---|---|
| 端点可用（no-scan） | 9/9 | 9/9 |
| 单轮抓取 | 2.8–3.6s | 0.9–1.6s |
| 登录页正则 | 匹配 | 匹配 |
| `newEncryptMode` | 1（SHA256） | 1（SHA256） |
| 慢端点 | `wifi_connect_devices` 2.9s、`wifi_detail_all` 2.4s | 无 |

## 多路由器支持

一个 exporter 可以同时采多台，每台独立登录、独立 profile、独立熔断与超时。

```json
{
  "routers": [
    {"ip": "192.168.31.1", "password": "…", "host": "living-room", "profile": "no-scan"},
    {"ip": "10.10.30.248", "password": "…", "host": "study",
     "profile": "default", "collectMode": "poll", "interval": "20s",
     "excludeEndpoints": ["wifi_list"], "timeout": "8s"}
  ]
}
```

容器场景用环境变量（JSON 数组）：

```bash
ROUTERS='[{"ip":"192.168.31.1","password":"…","host":"a"},{"ip":"10.10.30.248","password":"…","host":"b"}]' \
  ./miwifi-exporter
```

优先级：`ROUTERS` 环境变量 > `config.json` 的 `routers` 数组 >
旧的 `ROUTER_IP`/`ROUTER_PASSWORD`/`ROUTER_HOST` 单台写法。
**旧写法完全兼容**（三种路径均已实测通过）。

配置文件里也可以直接写时长字符串：`"routerTimeout": "8s"`。
完整示例见 [`config.json.example`](config.json.example)。

### 各台的指标集合应当一致

profile 决定采哪些接口，接口集合直接决定导出的指标集合。给不同��由器配不同
profile 会让它们**导出的指标不一样**，更糟的是真机验证时发现：某台用
`default`（含 `wifi_list`）会因为 WiFi 扫描阻塞整机而九个端点全挂，
另一台用 `no-scan` 则 9/9 全通。

exporter 会在启动时检查并告警：

```
[WARN] 各台采集的接口集合不一致 detail=full-31(default) 与 nscan-248(no-scan)
       采集的接口集合不一致 —— 相比 full-31 缺少: wifi_list；两台导出的指标集合
       会不同，建议统一 profile
```

`/routers` 端点的 `schemaAligned` 字段也会逐台标出。**建议所有路由器用同一个
profile**（推荐 `no-scan`）。

### 聚合指标恒定存在，按实体指标按需出现

跨路由器对比时，面板默认引用的是**聚合类**指标（cpu/memory/count/uptime/wan_*/mesh_*/…）。
这类指标**每台必定导出**，取不到就发 0，绝不因为某台没数据就整条消失——
否则对比面板上会出现一条有、一条没有的空洞。

`wan_negotiation_speed` 就是这么修的：实测发现两台里有一台没上报这个字段，
结果该指标只在一台存在。现在统一发 0，语义写在 HELP 里（0 = 路由器未上报）。

**按实体**的指标（`ipv4` / `device_*` / `wifi_detail` / `lan_port_link` /
`neighbor_wifi_rssi` / `wifi_device_signal`）不在此列：每个实体一条序列，
没有实体就没有序列，这是 Prometheus 的固有语义。真机实测下两台相差的只有
`wifi_device_signal`——那台 `study` 当时没有 WiFi 终端连接。

### ⚠️ 本版本唯一的破坏性变更：所有指标都加了 `host` 标签

原实现只有一半指标带 `host`（cpu/memory/count/uptime/wan/lan/wifi_sta/mesh），
另一半没有：`platform` / `version` / `sn` / `mac` / `ipv4*` / `ipv6` /
`device_*` / `wifi_detail` / `wifi_device_signal` / `neighbor_wifi_*`。

多路由器下，缺 `host` 的那半边会与另一台路由器的同名序列**真的撞车**，
Prometheus 在 Gather 阶段直接报错：

```
collected metric "miwifi_platform" {…} was collected before with
the same name and label values
```

也就是说，不补 `host`，多路由器根本无法实现——这不是取舍，是前提。

**对现有查询的影响**

| 查询写法 | 影响 |
|---|---|
| `miwifi_cpu_cores` | ✅ 不受影响 |
| `sum(miwifi_device_upload_traffic)` | ✅ 不受影响 |
| `miwifi_platform{platform="RB03"}` | ✅ 不受影响（没写 host 的匹配器照常匹配） |
| `miwifi_device_upload_traffic{ip="x",mac="y",device_name="z",is_ap="0"}` | ⚠️ **会失配**，需补 `host="…"` |
| `sum by (host) (...)` | 🆕 跨路由器聚合从此可用 |

**迁移**：给写死完整标签集的查询补上 `host`。只有明确写全了那 20 个指标的
所有标签时才会用到，实践中很少。

### 隔离性

一台出问题不影响另一台。真机实测（真机 + 一台不可达地址）：

```
miwifi_exporter_router_up{host="miwifi-31"}   1   ← 真机正常，511 条样本，0 超时
miwifi_exporter_router_up{host="dead-router"} 0   ← 不可达，36 次超时单独记账
```

各台有各自的 `stok`、熔断器、自适应降级状态与超时预算。`/routers` 可逐台查看
profile、启用的端点、被跳过的端点与超时次数。

> 多机下 `/metrics` 的耗时取**最慢那台**，不是各台之和（各台并发采集）。

## 采集模式

### `pull`（默认）

每次 Prometheus 抓取时同步向路由器取值。指标永远是抓取那一刻的真实值，语义与
旧版一致。十个接口**并发**发起，一次抓取的耗时约等于最慢的那个接口。

### `poll`

后台 goroutine 按 `COLLECT_INTERVAL` 周期采集，`/metrics` 只读内存快照。

路由器变慢或掉线时 `/metrics` 依然微秒级返回——实测 2ms 以内，而 `pull` 模式
需要等真实网络往返。代价是指标新鲜度由 `COLLECT_INTERVAL` 决定，而不再是
抓取时刻。

**选择建议**：单台家用路由器直连 → `pull`；exporter 跑在 K8s 里、而路由器是
家用宽带上的不稳定环节 → `poll`。

## HTTP 端点

| 路径 | 用途 |
|---|---|
| `/metrics` | Prometheus 指标（支持 gzip / OpenMetrics 内容协商） |
| `/livez` | 进程存活，恒 200 |
| `/readyz` | 就绪状态 |
| `/healthz` | 健康检查 |
| `/routers` | 逐台路由器的运行状态（JSON），排障用 |
| `/` | 首页（含各台状态表） |

> 探针**刻意不看路由器是否可达**。exporter 的价值恰恰在于路由器出问题时仍能
> 把「出问题」暴露成指标；把探针绑到路由器上，K8s 会把 Pod 摘出 Service，
> 结果连告警指标都抓不到。路由器健康度请看 `miwifi_exporter_router_up`。
>
> `/readyz` 只在 **poll 模式**下有实际含义（等首轮后台采集完成）；
> pull 模式任何时刻都能立刻采集，因此没有前置条件。未就绪时返回 503 并在
> 响应体里给出失败的检查项名与原因。

## exporter 自身指标

这一族是 v2 新增的（`_exporter_` 前缀，与路由器指标互不重名）。
旧版在路由器掉线时无法说明「到底哪里出了问题」——现在可以了。

| 指标 | 说明 |
|---|---|
| `miwifi_exporter_build_info{version,go_version}` | 构建信息 |
| `miwifi_exporter_scrape_duration_seconds{mode}` | 最近一次采集耗时 |
| `miwifi_exporter_scrape_total{mode,result}` | 采集次数（`success` / `partial` / `failure`） |
| `miwifi_exporter_endpoint_up{endpoint}` | 端点最近一次是否成功 |
| `miwifi_exporter_endpoint_stale{endpoint}` | 当前值是否为陈旧的历史值 |
| `miwifi_exporter_endpoint_errors_total{endpoint}` | 端点失败累计次数 |
| `miwifi_exporter_endpoint_duration_seconds{endpoint}` | 端点耗时 |
| `miwifi_exporter_endpoint_supported{endpoint}` | 当前固件是否提供该端点（0=已跳过） |
| `miwifi_exporter_snapshot_age_seconds` | 当前快照年龄（`poll` 模式下即新鲜度） |
| `miwifi_exporter_router_up{host}` | 该路由器最近一次是否可达（1/0） |

### 抓取超时会真的停下来

Prometheus 到了 `scrape_timeout` 直接断开时，exporter 会立刻中止正在进行的
路由器请求，而不是把十个接口白跑完。这靠的是把 HTTP 请求的 context 一路传到
采集链路（`prometheus.Collector.Collect` 的签名里没有 ctx，需要 HTTP 层注入）。

客户端自己断开**不算**路由器故障：这一轮不写自监控指标，`router_up` 保持上一次
的值。否则一次 Prometheus 超时就会把 `router_up` 打成 0，告警随之误报。

### 部分失败的行为

某个端点失败时，指标里暴露的是**上一次成功的值**，同时把该端点的
`endpoint_stale` 置 1。取舍是明确的：Prometheus 里宁可要一段「旧但连续」的
时间序列（`rate()` 不至于断档），也不要一片空洞；而陈旧与否由指标显式暴露，
不靠猜。

### 推荐告警

```promql
# 路由器不可达
miwifi_exporter_router_up == 0

# 部分接口异常，当前展示的是历史值
miwifi_exporter_endpoint_stale == 1

# 数据太旧（poll 模式下采集循环可能卡住）
miwifi_exporter_snapshot_age_seconds > 120

# 抓取持续失败
increase(miwifi_exporter_scrape_total{result!="success"}[10m]) > 0
```

## 路由器指标

namespace 默认为 `miwifi`，可用 `METRICS_NAMESPACE` 修改。

**所有指标都带 `host` 标签**——重构前有一半没有，为支持多路由器而补齐，
详见上文「破坏性变更」。

| 指标 | 标签 | 说明 |
|---|---|---|
| `cpu_cores` | `host` | CPU 核心数 |
| `cpu_mhz` | `host` | CPU 主频（MHz） |
| `cpu_load` | `host` | CPU 负载（0-1，部分固件恒为 0） |
| `memory_total_mb` | `host` | 内存总量（MB） |
| `memory_usage_mb` | `host` | 已用内存（MB） |
| `memory_usage` | `host` | 内存使用率（0-1） |
| `count_all` | `host` | 终端总数 |
| `count_online` | `host` | 在线终端数 |
| `count_all_without_mash` | `host` | 终端总数（不含 Mesh 节点） |
| `count_online_without_mash` | `host` | 在线终端数（不含 Mesh 节点） |
| `uptime` | `host` | 运行时长（秒） |
| `platform` | `host`,`platform` | 硬件平台标识，恒为 1 |
| `version` | `host`,`version` | 固件版本标识，恒为 1 |
| `sn` | `host`,`sn` | 序列号标识，恒为 1 |
| `mac` | `host`,`mac` | MAC 标识，恒为 1 |
| `ipv4` | `host`,`ipv4` | WAN IPv4，恒为 1 |
| `ipv4_mask` | `host`,`ipv4` | 子网掩码 CIDR 前缀位数 |
| `ipv6` | `host`,`ipv6` | WAN IPv6，恒为 1 |
| `wan_upload_speed` | `host` | WAN 上行速率（KB/s） |
| `wan_download_speed` | `host` | WAN 下行速率（KB/s） |
| `wan_upload_traffic` | `host` | WAN 上行累计流量（KB） |
| `wan_download_traffic` | `host` | WAN 下行累计流量（KB） |
| `wan_link_status` | `host` | WAN 链路状态码（2 = 已连接） |
| `wan_negotiation_speed` | `host` | WAN 协商速率（Mbps） |
| `wan_info` | `host`,`wan_ip`,`gateway`,`dns` | WAN 连接信息，恒为 1 |
| `lan_port_link` | `host`,`port` | LAN 口链路状态（0/1） |
| `lan_uptime` | `host` | LAN 运行时长（秒） |
| `device_upload_traffic` | `host`,`ip`,`mac`,`device_name`,`is_ap` | 终端累计上行流量（KB） |
| `device_upload_speed` | `host`,`ip`,`mac`,`device_name`,`is_ap` | 终端上行速率（KB/s） |
| `device_download_traffic` | `host`,`ip`,`mac`,`device_name`,`is_ap` | 终端累计下行流量（KB） |
| `device_download_speed` | `host`,`ip`,`mac`,`device_name`,`is_ap` | 终端下行速率（KB/s） |
| `device_online_time` | `host`,`ip`,`mac`,`device_name`,`is_ap` | 终端在线时长（秒） |
| `device_wifi_quality` | `host`,`ip`,`mac`,`device_name` | WiFi 信号等级（1-4） |
| `device_connection_type` | `host`,`ip`,`mac`,`device_name`,`type` | 连接类型，恒为 1 |
| `device_pcontrol` | `host`,`ip`,`mac`,`device_name` | 是否启用家长控制（0/1） |
| `wifi_detail` | `host`,`ssid`,`status`,`band_list`,`channel` | 各射频状态（1 = 开启） |
| `wifi_sta_count` | `host`,`band` | 各频段在线终端数 |
| `mesh_cap_count` | `host` | Mesh 主节点数 |
| `mesh_re_count` | `host` | Mesh 子节点数 |
| `wifi_device_signal` | `host`,`mac`,`band` | WiFi 终端信号强度（0-100） |
| `neighbor_wifi_rssi` | `host`,`ssid`,`bssid`,`band`,`channel` | 周围 WiFi 信号强度（dBm） |
| `neighbor_wifi_count` | `host` | 周围 WiFi 总数 |

### 指标基数

有几个指标的值来自用户可控的输入，值得留意序列规模：

| 指标 | 基数来源 | 建议 |
|---|---|---|
| `neighbor_wifi_rssi{ssid,bssid,band,channel}` | 周围 WiFi 的 SSID 与 BSSID | **最需要关注**。邻居增删会让序列持续churn，小内存的 Prometheus 上建议直接丢弃 |
| `wifi_detail{ssid,...}` | 本机各射频 SSID | 通常 ≤4 条，可放心用 |
| `device_*{device_name,mac,ip}` | 已连终端数 | 与实际终端数同阶 |
| `*_info{ipv4,ipv6,dns,...}` | WAN 地址，动态分配后会变 | 每次换 IP 就是一条新序列 |

丢掉噪声指标的两种方式（不改 exporter）：

```yaml
# scrape_configs
metric_relabel_configs:
  - source_labels: [__name__]
    regex: 'neighbor_wifi_rssi|neighbor_wifi_count'
    action: drop
```

```yaml
# ServiceMonitor
metricRelabelings:
  - sourceLabels: [__name__]
    regex: 'neighbor_wifi_.*'
    action: drop
```

如果你的场景确实要留 `neighbor_wifi_rssi` 做信道分析，注意它也会带来一个副作用：
**邻居数量本身会成为一个噪声信号**——图上看着像「干扰变严重」，其实只是旁边多了台设备。

## Grafana dashboard

https://grafana.com/grafana/dashboards/16557-xiaomi-router/

## 镜像

```bash
make docker                       # 本地构建
docker build --build-arg VERSION=$(git describe --tags) -t miwifi-exporter .
```

GitHub Actions（`.github/workflows/ci.yml`）在 `main` 分支与 `v*` tag 上推送时
构建并推送 `linux/amd64` / `linux/arm64` / `linux/arm/v7` 三架构镜像到
`ghcr.io/<repo>` 与 `fawenyo/miwifi-exporter`，并在推送前拉取 amd64 变体做一次
真实冒烟：`-version`、`缺配置必须非零退出`、`/metrics` 与四个探针端点、
自监控指标存在性、容器 healthcheck 状态。

> `.dockerignore` 排除了 `config.json`——它通常写着 routerPassword，
> 放进构建上下文会留在 builder 的层里，而 buildx 的 GHA 缓存会把它持久化。

## Kubernetes

```bash
helm install miwifi ./charts/miwifi-exporter \
  --set wifiSettings.routerIp=192.168.31.1 \
  --set wifiSettings.routerPassword=xxx
```

生产环境建议用已有 Secret，避免明文进入 helm release 记录：

```bash
kubectl create secret generic miwifi-router --from-literal=router-password=xxx

helm install miwifi ./charts/miwifi-exporter \
  --set wifiSettings.routerIp=192.168.31.1 \
  --set existingSecret=miwifi-router
```

启用 ServiceMonitor（需 Prometheus Operator）：

```bash
helm upgrade miwifi ./charts/miwifi-exporter --set serviceMonitor.enabled=true
```

> **网络可达性**：路由器在家庭内网，Pod 需要能直连它。若集群跨网段，
> 请确认 Pod 到路由器地址的路由可达，或改用 pull 模式的节点级部署。

## 开发

```bash
make help        # 查看全部目标
make test        # 全部测试（含 -race）
make cover       # 覆盖率报告
make lint        # gofmt + go vet
make build-all   # 交叉编译 linux 各架构
```

### 目录结构

```
main.go                 入口：flag/config → bootstrap.Run 生命周期
internal/config/        配置加载（env > json > default）与校验
internal/profile/       固件/型号差异的唯一存放处（新增版本只改这里）
internal/router/        路由器 API 客户端
  ├─ types.go             各接口的线格式模型
  ├─ auth.go              登录与 stok 获取
  ├─ client.go            带鉴权的请求、重试、熔断
  └─ collect.go           十个端点并发聚合 + 失败回退
internal/collector/     快照 → Prometheus 指标
  ├─ descs.go             指标描述符（指标契约的唯一定义处）
  ├─ collector.go         单台的采集与指标产出
  ├─ set.go               多台编排：唯一注册的 Collector，内部逐台扇出
  ├─ source.go            pull / poll 两种采集链路
  └─ self.go              exporter 自监控指标
internal/server/        Hertz 路由与中间件栈
internal/testutil/      测试用假路由器（不进入发布二进制）
```

### 测试

`internal/collector/compat_test.go` 里的 `legacyContract` 表是**逐条抄自重构前
实现的**指标名与标签集合。任何一次改动只要动了指标名、标签名或标签集合，这个
测试就会红——Prometheus 侧的 Grafana 面板与告警规则依赖这套名字，静默改名等于
静默让监控失效。新增指标请显式登记到该表。

| 测试 | 守住什么 |
|---|---|
| `compat_test.go` / `TestLegacyMetricContract` | 指标名与标签集合不变，且不多不少 |
| `compat_test.go` / `TestMetricHelpNotEmpty` | 每条指标都有非空 HELP |
| `compat_test.go` / `TestEndpointSupportedIsCorrectOnFirstGather` | 自监控指标不滞后一拍 |
| `context_test.go` / `TestRequestContextCancelsCollection` | 请求取消能中止采集 |
| `context_test.go` / `TestClientDisconnectDoesNotPolluteRouterHealth` | 客户端断开不污染 `router_up` |
| `config_test.go` / `TestPasswordNeverLeaks` | 密码不进日志 |
| `server_test.go` / `TestMetricsEndpointIsRawText` | `/metrics` 不被中间件包成 JSON |
| `server_test.go` / `TestReadinessReflectsCollectorState` | `/readyz` 真的反映就绪状态 |
| `profile_test.go` / `TestCustomParserIsPlugable` | 新固件加 parser 这条路走得通 |
| `router/profile_test.go` | 不支持的端点被跳过、自适应降级与复探 |

```bash
make test        # 全量 + race
make cover       # 覆盖率
```

## v2 重构说明

### 修复的问题

| 问题 | 旧行为 | 现在 |
|---|---|---|
| **启动日志打印明文密码** | `log.Println("密码: ", Configs.Password)` | 密码全程脱敏，有测试守住 |
| **stok 只在启动时取一次** | 路由器一重启，exporter 永久输出垃圾数据 | TTL 主动过期 + 401/403 自动重登，重登串行化 |
| **token 失效会杀掉进程** | 重登失败走 `log.Fatalf` → `os.Exit(1)` | 失败只降级该端点，进程继续服务 |
| **十次串行请求** | 一次 scrape = 十个 RTT | 并发发起，约等于最慢的那个 |
| **无超时** | `http.Client{}` 零值超时，路由挂了就永久挂住 | 每层都有超时（请求 / 采集 / 关停） |
| **正则结果直接取下标** | 固件改版即 `index out of range` panic | 全部返回 error |
| **近半数指标 HELP 为空** | `# HELP xxx ` 后面空白 | 全部补齐，有测试守住 |
| **无健康检查** | K8s 探针打 `/` | `/livez` `/readyz` `/healthz` |
| **无优雅关停** | `log.Fatal(ListenAndServe)` | SIGTERM 后排空在途请求 |
| **零测试** | — | 表驱动 + 假路由器，含 `-race` |

### 技术栈

| 层 | 选型 |
|---|---|
| 日志 | `oekit/logx`（kv 风格，包级 logger 用 `Delegating`） |
| 错误 | `oekit/pkg/errors`（`Wrap` + 哨兵） |
| 并发 | `oekit/concurrency`（`async` / `retry` / `breaker` / `singleflight`） |
| 配置 | `oekit/core/config`（`ApplyEnv` / `OverlayMerge` / `FillDefaults` / `ValidateTags`） |
| 生命周期 | `oekit/bootstrap`（信号监听 + 资源注册 + 后台任务） |
| 探针 | `oekit/bootstrap/healthz` |
| HTTP | `oekit/hertz` + `oekit/middleware`（TraceID → Recovery → Response → AccessLog） |
| 指标 | `prometheus/client_golang` |

> 选 `middleware` + `hertz` 而非 `httpx` 的理由：oekit 自己的决策树写得很清楚
> ——「调内部服务（要发现/熔断/重试）→ httpx；只是带 JSON 解析的 HTTP →
> pkg/httpjson」。这里是对一台固定地址的家用路由器发请求，用 `httpx` 属于杀鸡用牛刀。

## 故障排查

| 现象 | 处理 |
|---|---|
| 启动时打印 `sonic/ast only supports ... fallback to encoding/json` | bytedance/sonic 尚未适配超出 go.mod 声明的 Go 版本，不影响功能。镜像基于 `golang:1.26-alpine` 不会出现该提示 |
| 指标全是 0 或值不变 | 确认 `ROUTER_IP` 可达；看 `miwifi_exporter_router_up` 与 `miwifi_exporter_endpoint_up{endpoint=…}` 定位到具体接口 |
| 某个指标恒为 0，但路由器明明有 | 看 `miwifi_exporter_endpoint_supported`：0 表示这台机器压根没这接口（自适应探测已跳过），详见 [internal/profile](internal/profile/README.md) |
| 新固件登录失败 | 登录页结构变了，需要新增一个 `LoginPageParser`，见 [internal/profile/README.md](internal/profile/README.md#情况-b新固件登录页结构变了) |
| 日志刷 `端点采集失败` | 路由器在忙或 WiFi 信号差。已有熔断保护，不会拖垮 `/metrics`；可调大 `ROUTER_TIMEOUT` |
| Pod 被反复重启 | 检查是否误把 readiness 指向了 `/livez` 以外的路径；路由器不可达**不应**影响探针 |
| 配置明明写了却不生效 | 记住优先级是 flag > 环境变量 > config.json；用 `-validate-config` 打印最终生效值 |
| 日志刷太快 | 失败已按端点聚合成一行；想看逐端点错误用 `LOG_LEVEL=debug` |
| Prometheus 抓取超时 | 抓取超时会真的中止路由器请求（ctx 已贯通），调大 `SCRAPE_TIMEOUT` 或改用 `COLLECT_MODE=poll` |

## License

见 [LICENSE](LICENSE)。
