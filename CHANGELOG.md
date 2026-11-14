# Changelog

> **1.19.2 是当前最新版本**。它修的是一类「静默假零」缺陷：小米路由器
> stok 失效时回的是 HTTP 200 + `{"code":401}`，过去被当成一次成功采集，
> 于是指标大面积消失却毫无报错。1.19.0 的**破坏性变更**（一批聚合指标
> 改为「有条件导出」）仍然适用。

---

## 1.19.2

一次真机事故驱动的小版本修复。起因：2026-11-14 在 `10.10.10.31` 上，
另一个 exporter 实例登录顶掉了路由器的 admin 会话（小米路由器**只允许
一个 admin 会话**），线上实例的 stok 随即失效，于是 **144 条设备级序列
静默消失**——`/metrics` 仍然 200，指标变少，没有任何报错。

### 修复：HTTP 200 + code:401 被当成成功采集（P0）

小米路由器 stok 失效时**不**回 401/403，而是回 **HTTP 200 +
`{"code":401,"msg":"Invalid token"}`**（RA70 固件 1.0.140、R3P 固件
2.16.29 实测一致）。过去 `request()` 只看 HTTP 状态码，这个响应被当成
一次成功采集：

| 现象 | 原因 |
|---|---|
| `mac` / `sn` / `platform` / `version` 变空串 | 载荷解成全零结构 |
| `uptime` 变 0 | 同上 |
| 设备级序列整片消失 | 同上 |
| 端点被记成「成功」 | 状态码是 200 |
| **永远不重新登录** | 自愈只认 401/403 |

现在响应体里的 `code` 会被识别：401/403 归一成 `ErrStokInvalid`，正好
落进 `call()` 已有的「换新 stok 再试一次」自愈路径——那条路径本来就该
接住这种情况，只是过去永远等不到触发条件。其余非 0 code 归一成
`ErrAPIError`。

**副作用**：stok 失效后指标会**短暂断档**，而不是继续导出一批假 0。
这是有意的——假 0 比断档危险。

### 修复：不再对必然失败的请求重试（P0）

`ErrEndpointMissing`（404/501）、`ErrStokInvalid`、`ErrAPIError`
不再走通用重试。过去 404 会走满 3 次尝试 + 200/400ms 退避，白白多打
两次请求、多等 0.6s，而且与「404 一次就确定」的处置相矛盾；stok 失效
则是拿同一个已失效的 stok 撞 3 次墙。

### 修复：按端点分配的超时预算真正生效（P0）

`http.Client.Timeout`（`ROUTER_TIMEOUT`，默认 5s）是客户端级兜底，会把
按端点分配的预算（profile 内置的 `wifi_list=20s`、用户覆盖的
`lan_info=8s`）统统压回同一个值——「按端点给不同超时」在实现上等于
没做。现在去掉客户端级 Timeout，每条请求的 deadline 由 `request()` 里的
ctx 提供，端点级预算按预期生效。

### 性能：每轮采集的固定开销下降（P2）

| 项 | 改前 | 改后 |
|---|---|---|
| 端点表 | 每轮重建 10 结构体 + 30 闭包 | 构造期建一次 |
| `lastGood` 存值 | 存指向整份快照的指针 | 存值 |
| `wan_info` 解析 | 反序列化 → 再序列化 → 再反序列化 | `json.RawMessage` 直取 |
| 超时预算定位 | 每次遍历 10 项反查表 | O(1) 映射 |

基准（`BenchmarkFetchAll`，Apple M4，10 端点）：allocs/op 1294 → 1147
（-11.3%），B/op 129678 → 122245（-5.7%），延迟持平（该基准被 20ms 的
模拟端点延迟主导）。真正的收益在失败路径上：404 与 stok 失效每轮少打
2 次请求、少等 0.6s。

### 测试：假路由器对齐真机（P1）

`internal/testutil/fakerouter.go` 过去把 stok 失效建模成 403，于是整个
测试套件**从没跑过真机的真实形态**——这正是上面那个 P0 缺陷能在测试全绿
的情况下溜进线上的原因。现在默认回 200 + `{"code":401}`（真机形态），
`SetStokFailureStatus(403)` 可切回旧形态。

新增测试：`internal/router/apierror_test.go`（信封识别、前缀预判、空载荷
兜底）、`internal/router/resilience_test.go`（200+code:401 自愈且每端点
增量 ≤2 次请求、403 形态仍自愈、404 只请求 1 次、错误信封不算成功、
端点级超时不被客户端级压死）。

---

## 1.19.1

依赖升级：oekit 全模块 `pkg v0.13.0 -> v0.41.0`，无代码改动。

同机并排对比确认**无回归**：`10.10.10.31` 上 1.19.0 与 1.19.1 的
`/metrics` 序列数完全一致（317 vs 317）。

---

## ⚠️ 1.19.0 含破坏性变更

起因是一次真机巡检：对比 `10.10.30.71`（两台）与 `10.10.10.31`（一台）的
`/metrics`，发现后者的 42 个指标族里有 **9 个族一条序列都没有**，
另外几个族被稳定导出成 **0**——而那些 0 与「真的没有」完全无法分辨。

### 假零：端点不可用时不再导出 0

`router.Snapshot.Usable()` 统一了判定：**端点被 profile 跳过、或本轮失败
且没有陈旧值可沿用时，它的下游指标一条都不发**。

真机（10.10.10.31，采 R3P @10.10.10.249）上受影响的：

| 指标 | 来源端点 | 修复前 | 修复后 |
|---|---|---|---|
| `wifi_sta_count`（3 条 band） | `newstatus` | 恒 `0` | 不导出 |
| `mesh_cap_count` / `mesh_re_count` | `newstatus` | 恒 `0` | 不导出 |
| `device_wifi_quality` | `device_list` | 恒 `0` | 不导出 |
| `device_connection_type` / `device_pcontrol` | `device_list` | 恒 `0` | 不导出 |
| `neighbor_wifi_count` | `wifi_list` | 恒 `0` | 不导出 |
| `lan_port_link` / `lan_uptime` | `lan_info` | 恒 `0` | 不导出 |
| `wan_link_status` / `wan_negotiation_speed` / `wan_info` | `pppoe_status` | 恒 `0` / 空 | 不导出 |

**为什么是破坏性的**：跨台对比面板里，`sum(miwifi_wifi_sta_count)` 之类的
聚合查询会突然少掉一台机器的贡献，且不留任何痕迹。

**怎么区分「真的没有」和「没采到」**：用
`miwifi_exporter_endpoint_supported{endpoint="…"}`，值为 0 就是没采到。

**什么没变**：端点失败但有上次成功值时（`endpoint_stale=1`）**仍然导出**，
陈旧值好过让 `rate()` 断档，行为与 1.18.0 一致。

### `count_*_without_mash` 不再谎报 0

`Status.Count` 的两个 `*_without_mash` 字段从 `int` 改成 `*int`。

实测 R3P（固件 2.16.29）的 `/api/misystem/status` **只带 `all` 与 `online`**，
没有这两个字段。写成 `int` 的话 encoding/json 会把它们留成零值并原样导出，
于是同一台机器上同时出现：

```
miwifi_count_all{host="Home-GFW"}              104
miwifi_count_all_without_mash{host="Home-GFW"}  0   ← 固件压根没报这个字段
```

后者会被读成「除 Mesh 外 0 台设备」，与前者直接矛盾，且没有任何办法从指标
本身分辨真假。现在字段缺失就不发序列。

### `wan_info` 推翻 1.18.0 的「刻意不动」

1.18.0 把 `wan_info{wan_ip="",gateway="",dns=""}` 归入「聚合指标恒定存在」
并明确保留，理由是「`wan_link_status` 已完整表达 WAN 通断」。**这个理由
本身站不住**：空地址的 `wan_info` 与同一版本删掉的 `ipv4{ipv4=""}` 是同一类
空占位记录。现在地址/网关/DNS 全空时不再导出。

「WAN 没连」这个事实没有因此丢失——它由 `wan_link_status=0` 表达，
`wan_info` 本就不该承担这个语义。

### 新增：重复失败降级

停问端点的依据从「只有 404/501」扩展到两条：

1. HTTP 404 / 501——一次就确定。
2. **连续 5 轮吐出逐字相同的非超时错误**——`profile.RepeatedFailureThreshold`。

第 2 条来自一次真机排查：`10.10.10.31` 上 `pppoe_status` 反复失败但不是 404，
自适应探测永远不收敛，`scrape_total{result="partial"}` 只增不减（实测 10 次
采集 0 次 success），而 `endpoint_supported` 始终说 1。

**判据是错误文本完全相同，不是失败次数**——这是安全阀。超时与
**熔断打开**都被显式排除：wifi_list 触发 WiFi 扫描会阻塞整机并打开熔断，
随后九个健康端点轮流拿到逐字相同的 `circuit breaker is open`，不排除的话
扫描一次就会把整台路由器的端点全部停问（`RecordResult` 注释里警告的那场事故，
换了个入口重来一遍）。

被停问的端点仍按 30 分钟复探，固件升级后新长出来的接口不会被永久忘掉。

### 新增：`/routers` 暴露 `endpointErrors`

```bash
curl -s http://10.10.10.31:9001/routers | jq '.[] | {host, endpointErrors}'
```

指标能告诉你「哪个端点挂了」，但说不出**为什么挂**——而失败原因此前只出现在
容器日志里，排查的人却未必有那台机器的 ssh。挂到 HTTP 上之后一条 curl 就能拿到。

### 修复：配置里的 `excludeEndpoints` 会在 30 分钟后被静默撤销

这是本次真机部署时才暴露出来的 bug，也是本次排查里**最隐蔽的一个**。

R3P 的 compose 里明明写着：

```json
{"ip":"10.10.10.249","host":"Home-GFW","profile":"no-scan","excludeEndpoints":["pppoe_status"]}
```

启动日志也确实打了「按配置排除端点 endpoints=[pppoe_status]」，
`/routers` 第一次也显示它已被跳过。**一切看上去都对。**

但 4.5 小时后的容器日志里：

```
[INFO] 重新探测此前判定为不存在的端点 endpoints=[device_list newstatus pppoe_status]
[WARN] pppoe_status … status=500 body=Internal Server Error
```

**根因**：`Disable()` 有两种来源，却共用同一份状态——

- 探测判定「这台固件没有这个接口」→ 应该复探（固件升级后接口可能回来）
- 配置显式排除 → **是用户的明确意志，不该被任何自动机制推翻**

`RecheckDue()` 无差别地把两者一起放回探测名单。于是配置排除在 30 分钟后
失效，`pppoe_status` 重新开始每轮报错，`scrape_total{result="partial"}`
只增不减——而配置文件里那行字从头到尾没变过，**配置形同虚设且毫无提示**。

**修复**：新增 `Instance.Pin()`，`RetryDisabled` 跳过被钉死的端点。
配置排除经由 `router.New` 的 `ExcludeEndpoints` 走 `Pin`，永久生效；
探测结论仍走 `Disable`，保留 30 分钟复探。解除钉死只需重启进程。

### 顺带确认：真实错误是 500，不是 404

```
status=500 body=Internal Server Error
```

`pppoe_status` 在这台 R3P 上是 500，而 `device_list` / `newstatus` 才是 404。
上面那条「连续 5 轮相同错误降级」正好命中——错误文本逐字相同
（`路由器返回非 2xx: … status=500 body=Internal Server Error [pppoe_status]`），
第 5 轮自动停问。两条机制叠加，这个端点现在无论如何都收敛了。

---

## ⚠️ 1.18.0 含破坏性变更

**没有实体就没有序列**——这句话此前只写在 README 里，这次真的落到了代码上。

### 哪些序列会消失

真机（10.10.30.71，同时采 `192.168.31.1` 与 `10.10.30.248`）上实测会消失的：

```
miwifi_ipv4{host="…",ipv4=""}                 ← 10.10.30.248 的空占位记录
miwifi_ipv4_mask{host="…",ipv4=""}             ← 同上，且值 0 会被读成 /0
miwifi_device_upload_traffic{mac="",device_name="Others",ip="",is_ap=""}
miwifi_device_download_traffic{mac="",device_name="Others",ip="",is_ap=""}
```

涉及指标：`ipv4`、`ipv4_mask`、`ipv6`、`device_upload_traffic`、
`device_download_traffic`、`device_wifi_quality`、`device_connection_type`、
`device_pcontrol`、`wifi_device_signal`、`neighbor_wifi_rssi`。

### 为什么是破坏性的

上面每一条都是**空标签序列**——所有标签为空，没有任何东西可以用来定位它。
它们不携带信息，却会稳定占住一条时间序列，并且在 `sum by (mac)` / `sum by (ipv4)`
这类查询里混进一个空桶。留着比缺一条更难排查：`count(miwifi_device_upload_traffic)`
会把它们算成「一台设备」。

### 什么没有变

- **指标名、标签集合、取值语义全部没动**，没有任何 PromQL 会因此写错。
- **有身份的设备一条都没少。** MAC 有效但查不到 IP 的终端仍然导出，
  `ip=""` 是在诚实地说「IP 未知」，不是「这台机器不存在」。
- 只影响恰好在查「空标签序列」的查询——这类查询本来就没有意义。

### 排查

升级后若某条查询结果变少，先确认它是不是在数「空 mac / 空 ipv4 的序列」。
要按设备维度查，请用 `mac` 或 `device_name` 匹配，别用 `ipv4 != ""` 这类条件。

### `ipv4_mask` 不再谎报 /0

子网掩码解析不出来时，旧代码只 Warn 然后照样 `emit(0)`。0 会被读成 `/0` 网段——
一个根本不存在的巨大网段。现在该序列直接不发。

`subnetMaskToLen` 本身也修了一处：`net.IPMask.Size()` 对非连续掩码
（如 `255.0.255.0`）返回 `(0, 0)` **且不报错**，旧代码的 `ones, _ :=`
把这类掩码也当成了 0。现在反查 `net.CIDRMask(ones, 32)` 确认是规范化形式。

### `wan_info` 刻意不动

`wan_info{wan_ip="",gateway="",dns=""}` 同样是空标签序列，但**保持原样**：
README 已把它归入「聚合指标恒定存在，取不到也发 0」这一类，且
`wan_link_status{host}`（2 = 已连接）已经完整表达了「WAN 通不通」。
为了少数几台 WAN 异常的机器去掉一条承诺过的序列，不划算。

### 其他

- 修 `make build-all`：`GOARCH=armv7` 不是合法取值，该目标此前必然失败。
  改为 `GOARCH=arm GOARM=7`。
- 新增 `deploy/arm64-local.Dockerfile`：无 Go 环境的 ARM 设备（如树莓派）
  可用它把交叉编译好的二进制打成镜像，不必推 registry。

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

### 指标语义修正：`router_up`

`router_up{host}` 的含义从「所有端点都成功」改为「**这台路由器我们采到数据了吗**」。

真机上踩到的：一台 R3P 老机器不支持 `pppoe_status`，于是 7 个端点里 1 个失败
就把整台判成不可达，`router_up` 恒为 0、告警一直误报——而那台的数据完全正常。

端点级健康本来就有 `endpoint_up{host,endpoint}` 表达，不必再叠一层到整机指标上。

- `router_up == 0` 现在**只**在「一个端点都没采到」时出现
- `scrape_total{result="success"|"partial"}` 仍按严格口径分档，供排障使用

**如果你在 1.17.0/1.17.1 上配过 `router_up == 0` 告警，升级后触发会明显变少**
——那是因为之前在误报。

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
