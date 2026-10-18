# miwifi-exporter

小米路由器（MiWiFi）的 Prometheus exporter。

[![CI](https://github.com/zy84338719/miwifi_exporter/actions/workflows/ci.yml/badge.svg)](https://github.com/zy84338719/miwifi_exporter/actions/workflows/ci.yml)
[![Version](https://img.shields.io/badge/version-1.18.0-orange)](CHANGELOG.md)
[![Go](https://img.shields.io/badge/go-1.26%2B-blue)](https://go.dev)
[![License](https://img.shields.io/badge/license-Apache--2.0-green)](LICENSE)

一个进程采多台路由器，每台独立登录、独立固件适配、独立熔断与超时。

![grafana](screenshot-2.png)

> ### ⚠️ 从 1.17.x 升级到 1.18.0 请先看这里
>
> 1.18.0 的破坏性变更只有一条：**空标签序列不再导出**（`ipv4=""`、
> `mac="" device_name="Others"` 这类）。指标名、标签集合、取值语义全部没动，
> 没有任何 PromQL 会因此写错；只是恰好在数「空标签序列」的查询会变少。
>
> 原因与完整清单见 [CHANGELOG](CHANGELOG.md) 的 1.18.0 一节。

> ### ⚠️ 从 1.16.0 升级到 1.17.0 请先看这里
>
> 本版本有一个**破坏性变更**：所有指标新增 `host` 标签。
>
> 绝大多数 PromQL 不受影响（不写标签的匹配器照常匹配）；受影响的只有
> **写死完整标签集**的精确匹配式，补上 `host="…"` 即可：
>
> ```
> # 升级前
> miwifi_device_upload_traffic{ip="192.168.31.100", mac="…", device_name="…", is_ap="0"}
> # 升级后
> miwifi_device_upload_traffic{host="living-room", ip="192.168.31.100", mac="…", device_name="…", is_ap="0"}
> ```
>
> 指标名本身一个都没改。完整迁移说明见 [CHANGELOG](CHANGELOG.md) 顶部。

---

## 30 秒上手

```bash
export ROUTER_IP=192.168.31.1
export ROUTER_PASSWORD=你的路由器密码
export ROUTER_PROFILE=no-scan          # 强烈建议，见「真机行为」
go run .                              # 或 make build 后跑 ./bin/miwifi-exporter
curl http://localhost:9001/metrics
```

Docker：

```bash
docker run -d --name miwifi-exporter -p 9001:9001 \
  -e ROUTER_IP=192.168.31.1 -e ROUTER_PASSWORD=xxx -e ROUTER_PROFILE=no-scan \
  ghcr.io/zy84338719/miwifi_exporter:latest
```

Helm：

```bash
helm install miwifi ./charts/miwifi-exporter \
  --set wifiSettings.routerIp=192.168.31.1 \
  --set wifiSettings.routerPassword=xxx
```

---

## 为什么默认推荐 `ROUTER_PROFILE=no-scan`

这条经验来自真机实测，是本项目最值得先知道的一件事。

`/api/xqnetwork/wifi_list` 会触发一次 WiFi 扫描，**要 13–14 秒**；更关键的是
**扫描期间路由器会阻塞其余全部 LuCI 请求**。后果不是「这个指标慢」，而是
另外九个本来 0.2 秒就返回的端点跟着一起超时：

| 配置 | 抓取耗时 | 端点可用 |
|---|---|---|
| 默认（含 `wifi_list`） | 5.0s（被超时钉死） | **1/10** |
| `ROUTER_PROFILE=no-scan` | **1–3s** | **9/9** |

`no-scan` 只是摘掉 `wifi_list`，其余九个端点一个不少。代价是失去
`neighbor_wifi_rssi` / `neighbor_wifi_count` 两个指标——需要的话用
`ROUTER_PROFILE=default ENDPOINT_TIMEOUTS=wifi_list=20s` 打开，并接受慢抓取。

详见 [真机行为](#真机行为) 与 [`internal/profile/README.md`](internal/profile/README.md)。

---

## 配置

优先级：**命令行 flag > 环境变量 > config.json > 内置默认值**。

> 环境变量压过配置文件是刻意保留的旧行为：已经用 Helm / compose 通过环境变量
> 部署的实例不会因为某个 config.json 而行为突变。

### 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `ROUTER_IP` | **必填** | 路由器地址，支持 `192.168.31.1` / `:8080` / `http://…` |
| `ROUTER_PASSWORD` | **必填** | 后台密码（不会出现在日志里） |
| `ROUTER_HOST` | `miwifi` | `host` 标签的取值 |
| `ROUTER_PORT` | `9001` | exporter 自身监听端口 |
| `ROUTER_TIMEOUT` | `5s` | 单次 HTTP 请求超时 |
| `ROUTER_PROFILE` | `auto` | `auto`/`default`/`minimal`/`no-scan`/`no-xqnetwork` |
| `EXCLUDE_ENDPOINTS` | 空 | 无条件跳过的端点，如 `wifi_list` |
| `ENDPOINT_TIMEOUTS` | 空 | 按端点放宽超时，如 `wifi_list=20s` |
| `ENDPOINT_TIMEOUT_THRESHOLD` | `3` | 连续超时的告警阈值（仅观测，不停问） |
| `METRICS_PATH` | `/metrics` | 指标暴露路径 |
| `METRICS_NAMESPACE` | `miwifi` | 指标名前缀 |
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `COLLECT_MODE` | `pull` | `pull` 或 `poll` |
| `COLLECT_INTERVAL` | `30s` | poll 模式采集间隔 |
| `SCRAPE_TIMEOUT` | `15s` | pull 模式单次采集上限 |
| `SHUTDOWN_TIMEOUT` | `10s` | 优雅关停预算 |
| `ROUTERS` | 空 | **多台**路由器的 JSON 数组（优先于单台字段） |

### 命令行 flag

| flag | 说明 |
|---|---|
| `-config` | 配置文件路径，默认 `config.json`（不存在不算错误） |
| `-web.telemetry-path` | 同 `METRICS_PATH` |
| `-metric.namespace` | 同 `METRICS_NAMESPACE` |
| `-collect-mode` | 同 `COLLECT_MODE` |
| `-version` | 打印版本后退出 |
| `-validate-config` | 只校验配置不启动（适合放进容器启动前检查） |

完整示例见 [`config.json.example`](config.json.example)。

---

## 多路由器

```json
{
  "routers": [
    {"ip": "192.168.31.1", "password": "…", "host": "living-room", "profile": "no-scan"},
    {"ip": "10.10.30.248", "password": "…", "host": "study",
     "profile": "no-scan", "collectMode": "poll", "interval": "20s",
     "excludeEndpoints": ["wifi_list"], "timeout": "8s"}
  ]
}
```

容器场景用环境变量：

```bash
ROUTERS='[{"ip":"192.168.31.1","password":"…","host":"a","profile":"no-scan"},
           {"ip":"10.10.30.248","password":"…","host":"b","profile":"no-scan"}]' \
  ./miwifi-exporter
```

三种写法优先级：`ROUTERS` 环境变量 > `config.json` 的 `routers` 数组 >
旧的 `ROUTER_IP`/`ROUTER_PASSWORD`/`ROUTER_HOST` 单台写法（**完全兼容**）。

**一个进程采多台**（推荐）：

```bash
docker compose -f docker-compose.multi.yml up -d
```

相比每台一个容器：内存与进程减半、日志只有一份、`/routers` 一屏看全所有路由器，
抓取耗时取最慢那台而非各台之和。代价有两条，读之前先想清楚：

1. **只暴露一个端口**。Prometheus 原来若配了多个 target 需合成一个，
   跨路由器聚合改用 `sum by (host) (...)`（不写标签的查询不受影响）。
2. **故障域变大**：进程崩溃时所有路由器一起中断。路由器之间仍然隔离——
   一台不可达不会影响另一台的 `router_up`。

配置文件里可以直接写时长字符串：`"routerTimeout": "8s"`。

### 为什么所有指标都有 `host` 标签

**`host` 是多路由器的硬前提**——原来只有一半指标带它，缺的那些在多台时会真的
撞车，Prometheus 在 Gather 阶段直接报 `duplicate metrics`。

绝大多数 PromQL 不受影响；受影响的只有写死完整标签集的精确匹配式。
完整迁移说明见 [CHANGELOG](CHANGELOG.md)。

### 隔离性

各台有各自的 `stok`、熔断器、自适应降级状态与超时预算。真机实测（真机 +
一台不可达地址）：

```
miwifi_exporter_router_up{host="miwifi-31"}   1   ← 真机正常，511 条样本，0 超时
miwifi_exporter_router_up{host="dead-router"} 0   ← 不可达，36 次超时单独记账
```

多机下 `/metrics` 的耗时取**最慢那台**，不是各台之和。

### 各台指标集合应当一致

profile 决定采哪些接口，接口集合直接决定导出的指标集合。给不同路由器配不同
profile 会让它们**导出的指标不一样**，更糟的是真机验证时发现某台用 `default`
会因为 WiFi 扫描阻塞整机而九个端点全挂。

启动时会检查并告警：

```
[WARN] 各台采集的接口集合不一致 detail=full-31(default) 与 nscan-248(no-scan)
       采集的接口集合不一致 —— 相比 full-31 缺少: wifi_list；建议统一 profile
```

`/routers` 端点的 `schemaAligned` 字段逐台标出。**建议所有路由器用同一个 profile。**

### 聚合指标恒定存在

跨台对比面板默认引用**聚合类**指标（cpu/memory/count/uptime/wan_\*/mesh_\*…）。
这类每台必定导出，取不到就发 0，绝不因为某台没数据就整条消失。
`wan_negotiation_speed` 就是因此修的（实测两台里有一台没上报该字段）。

**按实体**的指标（`ipv4` / `ipv4_mask` / `ipv6` / `device_*` / `wifi_detail` /
`lan_port_link` / `neighbor_wifi_rssi` / `wifi_device_signal`）不在此列：每个实体一条序列，
没有实体就没有序列，这是 Prometheus 固有语义。

> 从 **1.18.0** 起这条规则真的落到了代码上。某些固件在拿不到数据时会返回
> 空占位记录——`wan_info.ipv4` 里一条 `ip`/`mask` 全空的项，`status.dev` 里一条
> `mac="" device_name="Others"` 的汇总项。这些不是实体，导出成空标签序列后
> 既无法定位，又会在 `sum by (mac)` 里混进一个空桶。现在一律跳过。
>
> 有身份但字段缺失的设备**仍然导出**：MAC 有效而查不到 IP 时 `ip=""`，
> 意思是「IP 未知」，不是「这台机器不存在」。按设备维度查询请用
> `mac` 或 `device_name` 匹配。
>
> 例外是 `wan_info`：它虽可能出现空标签，但已被归入上面的「聚合指标恒定存在」，
> 且 `wan_link_status{host}` 完整表达了 WAN 通断，刻意保持原样。

---

## HTTP 端点

| 路径 | 用途 |
|---|---|
| `/metrics` | Prometheus 指标（支持 gzip / OpenMetrics 协商） |
| `/routers` | 逐台路由器的运行状态（JSON），排障用 |
| `/livez` | 进程存活，恒 200 |
| `/readyz` | 就绪状态（poll 模式下等首轮采集完成） |
| `/healthz` | 健康检查 |
| `/` | 首页（含各台状态表） |

> 探针**刻意不看路由器是否可达**。exporter 的价值恰恰在于路由器出问题时仍能
> 把「出问题」暴露成指标；把探针绑到路由器上，K8s 会把 Pod 摘出 Service，
> 结果连告警指标都抓不到。路由器健康度请看 `miwifi_exporter_router_up{host}`。

抓取超时会**真的中止**正在进行的路由器请求（ctx 全链路贯通），而不是把十个
接口白跑完。客户端自己断开也不算路由器故障——那一轮不写自监控指标，
`router_up` 保持上次的值。

---

## 采集模式

| 模式 | 说明 | 实测 `/metrics` |
|---|---|---|
| `pull`（默认） | 每次 scrape 同步取数，十端点并发 | 1–3s |
| `poll` | 后台周期采集，scrape 只读内存快照 | **1ms** |

**选哪个**：单台家用路由器直连 → `pull`；exporter 跑在 K8s 里、而路由器是家用
宽带上的不稳定环节 → `poll`。

---

## 真机行为

以下结论来自对两台真实小米路由器的实测（`192.168.31.1` 与 `10.10.30.248`），
**假路由器测不出来**——它们每个接口都是瞬间返回的。

| | 192.168.31.1 | 10.10.30.248 |
|---|---|---|
| 端点可用（no-scan） | 9/9 | 9/9 |
| 单轮抓取 | 2.8–3.6s | 0.9–1.6s |
| 登录页正则 | 匹配 | 匹配 |
| `newEncryptMode` | 1（SHA256） | 1（SHA256） |
| 慢端点 | `wifi_connect_devices` 2.9s、`wifi_detail_all` 2.4s | 无 |

`wifi_list` 阻塞整机的问题见 [开头](#为什么默认推荐-router_profileno-scan)。

还有一个反面教训写在了代码注释和 `internal/profile/README.md` 里：**「连续超时
就停问端点」是错的启发式**。超时无法区分「这个端点坏了」与「路由器被别的端点
堵住了」，实测 8 次抓取里它误杀了 7 个健康端点。现在超时只做观测，停问只认
404/501 这种一次性确定的事实。

---

## 指标

### 路由器指标

namespace 默认为 `miwifi`，可用 `METRICS_NAMESPACE` 修改。**所有指标都带 `host` 标签。**

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
| `wan_negotiation_speed` | `host` | WAN 协商速率（Mbps）；0 = 路由器未上报 |
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

### exporter 自身指标

`<ns>_exporter_*` 前缀，全部带 `host`。这一族回答的是「到底哪里出了问题」。

| 指标 | 说明 |
|---|---|
| `build_info{version,go_version}` | 构建信息（进程级，只有一份） |
| `router_up{host}` | 该路由器最近一次是否可达（1/0） |
| `endpoint_up{host,endpoint}` | 端点最近一次是否成功 |
| `endpoint_stale{host,endpoint}` | 当前值是否为历史遗留的陈旧值 |
| `endpoint_supported{host,endpoint}` | 当前 profile 是否认为本机提供该端点 |
| `endpoint_errors_total{host,endpoint}` | 端点失败累计次数 |
| `endpoint_duration_seconds{host,endpoint}` | 端点耗时 |
| `scrape_duration_seconds{host,mode}` | 最近一次采集耗时 |
| `scrape_total{host,mode,result}` | 采集次数（`success`/`partial`/`failure`） |
| `snapshot_age_seconds{host}` | 快照年龄（poll 模式下即新鲜度） |

### 部分失败的行为

某个端点失败时，指标里暴露的是**上一次成功的值**，同时把该端点的
`endpoint_stale` 置 1。取舍是明确的：Prometheus 里宁可要一段「旧但连续」的
时间序列（`rate()` 不至于断档），也不要一片空洞；陈旧与否由指标显式暴露，不靠猜。

首次就失败（无历史值）的端点 `endpoint_stale=0`，即「压根没采到」。

### 推荐告警

```promql
# 路由器不可达
miwifi_exporter_router_up{host="living-room"} == 0

# 部分接口异常，当前展示的是历史值
miwifi_exporter_endpoint_stale == 1

# 数据太旧（poll 模式下采集循环可能卡住）
miwifi_exporter_snapshot_age_seconds > 120

# 抓取持续失败
increase(miwifi_exporter_scrape_total{result!="success"}[10m]) > 0
```

### 指标基数

`neighbor_wifi_rssi{ssid,bssid,…}` 的值来自周围 WiFi，**邻居增删会让序列持续
churn**，小内存的 Prometheus 上建议直接丢弃：

```yaml
# ServiceMonitor
metricRelabelings:
  - sourceLabels: [__name__]
    regex: 'neighbor_wifi_.*'
    action: drop
```

留着它还有个副作用：**邻居数量本身会伪装成「干扰变严重」**，其实只是旁边多了台设备。

---

## Grafana

https://grafana.com/grafana/dashboards/16557-xiaomi-router/

---

## 镜像

```bash
make docker
docker build --build-arg VERSION=$(git describe --tags) -t miwifi-exporter .
```

GitHub Actions 在 `main` 分支与 `v*` tag 推送时构建
`linux/amd64` / `linux/arm64` / `linux/arm/v7` 三架构镜像，推到
`ghcr.io/zy84338719/miwifi_exporter` 与 `fawenyo/miwifi-exporter`，
并在推送前拉取 amd64 变体做真实冒烟：`-version`、缺配置必须非零退出、
`/metrics` 与四个探针、自监控指标存在性、容器 healthcheck 状态。

> `.dockerignore` 排除了 `config.json`——它通常写着 routerPassword，
> 放进构建上下文会留在 builder 的层里，而 buildx 的 GHA 缓存会把它持久化。

---

## Kubernetes

```bash
helm install miwifi ./charts/miwifi-exporter \
  --set wifiSettings.routerIp=192.168.31.1 \
  --set wifiSettings.routerPassword=xxx
```

**多台**建议把含 `routers` 数组（含密码）的 config.json 放进 Secret，
避免明文进入 helm values 与 release 记录：

```bash
kubectl create secret generic miwifi-routers --from-file=config.json=./routers.json
helm install miwifi ./charts/miwifi-exporter \
  --set wifiSettings.routersConfigSecret=miwifi-routers
```

ServiceMonitor（需 Prometheus Operator）：

```bash
helm upgrade miwifi ./charts/miwifi-exporter --set serviceMonitor.enabled=true
```

> **网络可达性**：路由器在家庭内网，Pod 需能直连它。跨网段部署请确认路由可达，
> 或改用节点级部署。

---

## 开发

```bash
make help        # 全部目标
make test        # 全量 + race
make cover       # 覆盖率
make lint        # gofmt + go vet
make build-all   # 交叉编译 linux 各架构
```

### 目录结构

```
main.go              flag/config → bootstrap.Run 生命周期
internal/config/     env > json > default 三级优先级 + 校验；routers 多机解析
internal/profile/    固件差异的唯一存放处（端点/散列/登录页/频段映射）
internal/router/     路由器客户端：stok 自愈、并发采集、熔断、退避重试
internal/collector/  快照 → 指标；Set 是唯一注册的 Collector，内部逐台扇出
internal/server/     Hertz 路由 + oekit 中间件栈 + healthz 探针
internal/testutil/   测试用假路由器（可注入延迟/404/故障，不进发布二进制）
```

### 测试

104 个测试函数，覆盖率 77.3%，全部 `-race` 通过。

| 测试 | 守住什么 |
|---|---|
| `collector/compat_test.go` `TestLegacyMetricContract` | 指标名与标签集合与旧版逐条一致 |
| `collector/compat_test.go` `TestEndpointSupportedIsCorrectOnFirstGather` | 自监控指标不滞后一拍 |
| `collector/set_test.go` `TestAggregateMetricsPresentForEveryRouter` | 各台聚合指标集合一致 |
| `collector/compat_test.go` `TestEmptyOrBrokenWANAddressIsNotExported` | 空地址 / 坏掩码不产出序列，也不谎报 /0 |
| `collector/compat_test.go` `TestIdentitylessEntityIsNotExported` | 「Others」这类无身份记录不伪装成设备 |
| `collector/set_test.go` `TestSetOneSlowRouterDoesNotBlockOthers` | 慢机不拖累快机 |
| `collector/context_test.go` `TestClientDisconnectDoesNotPolluteRouterHealth` | 客户端断开不污染 `router_up` |
| `collector/set_test.go` `TestEndpointErrorCounterIncrements` | 端点失败路径不打 panic |
| `config/config_test.go` `TestPasswordNeverLeaks` | 密码不进日志 |
| `server/server_test.go` `TestMetricsEndpointIsRawText` | `/metrics` 不被中间件包成 JSON |
| `profile/realhw_test.go` `TestSlowEndpointsIsEmptyByDefault` | 不重蹈「慢端点放宽超时」的覆辙 |

> ⚠️ `concurrency/async` 会 **recover** 掉 panic。凡是它包着的代码，
> 「测试通过」证明不了没有 panic——曾经一个少传标签参数的 panic 被静默恢复，
> 那台路由器的指标凭空消失而 `/metrics` 照样 200、所有测试照样全绿。
> 现在这类路径必须有显式断言。

---

## 故障排查

| 现象 | 处理 |
|---|---|
| 启动日志打印明文密码 | 不会——已脱敏，有测试守着。旧版本（v1）会 |
| 指标全是 0 或值不变 | 看 `miwifi_exporter_router_up{host}` 与 `endpoint_up{host,endpoint}` 定位接口 |
| 某个指标恒为 0，但路由器明明有 | 看 `endpoint_supported`：0 表示这台机器压根没这接口（已跳过） |
| 抓取 5 秒且大量端点超时 | 十有八九是 `wifi_list` 在触发扫描。设 `ROUTER_PROFILE=no-scan` |
| 新固件登录失败 | 登录页结构变了，需要新增 `LoginPageParser`，见 [`internal/profile/README.md`](internal/profile/README.md#情况-b新固件登录页结构变了) |
| 日志刷太快 | 失败已按端点聚合成一行；想看逐端点错误用 `LOG_LEVEL=debug` |
| Prometheus 抓取超时 | 抓取超时会真的中止路由器请求；调大 `SCRAPE_TIMEOUT` 或改用 `COLLECT_MODE=poll` |
| Pod 被反复重启 | 检查是否误把 readiness 指向 `/livez` 以外的路径；路由器不可达**不应**影响探针 |
| 配置明明写了却不生效 | 优先级是 flag > 环境变量 > config.json；用 `-validate-config` 看最终生效值 |

---

## License

[Apache-2.0](LICENSE)。原项目作者：[helloworlde/miwifi-exporter](https://github.com/helloworlde/miwifi-exporter)。
