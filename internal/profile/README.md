# 固件适配（Profile）

> 这份文档面向「以后要支持新固件」的人。日常使用不需要看这里。

一句话：`internal/profile` 包是**唯一**存放「随路由器型号/固件版本而变」的地方。
遇到新固件，改这个包，不要去翻 `router/` 或 `collector/`。

## 为什么要有这个包

重构前，所有版本差异是散在代码里的：

```go
// auth.go —— 散列算法写死
password := hashSHA256(...)  // 只支持 SHA256 的固件

// auth.go —— 正则写死
webKeyRe = regexp.MustCompile(`key:.*?'(.*?)',`)

// client.go —— 十个端点无条件全打一遍
for _, ep := range allTenEndpoints { c.call(ctx, ep) }
```

于是每来一台新机器，问题都长这样：

- 登录页结构变了 → panic（正则取下标 `[1]`）
- 路由器没有 `/api/xqnetwork/wifi_list` → 每轮 scrape 打一条 WARN，指标恒为 0
- 双频段机器把未用射频报成 `wifiIndex=3` → 被错标成 `5GHz-high`
- 固件新增 `newEncryptMode=2` → 悄悄用 SHA1 去撞 SHA256 的门

现在这些都归到一处了。

## 收敛了哪四类差异

| 差异 | 谁负责 | 文件 |
|---|---|---|
| 哪些接口真实存在 | `Profile.Endpoints()` | `builtin.go` |
| `newEncryptMode` → 散列算法 | `Profile.Hasher()` | `builtin.go` |
| 登录页 HTML 结构 | `Profile.LoginPageParser()` | `login.go` |
| `wifiIndex` → 频段名 | `Profile.BandFor()` | `builtin.go` |

外加一份运行期状态（哪些接口实测不存在），在 `profile.go` 的 `Instance`。

## 包结构

```
internal/profile/
  profile.go   Profile 接口、端点常量、Instance（运行期状态机）、注册表
  builtin.go   ← 新增固件版本时的主要落点
  login.go     登录页 / 登录响应解析（固件差异最大、最易变的一环）
  hash.go      SHA1 / SHA256 原语
  detect.go    profile 选择 + 自适应探测
```

## 内置 profile

| 名字 | 端点 | 适用 |
|---|---|---|
| `auto`（默认） | 全部 10 个，逐个探测收敛 | 新旧通吃 |
| `default` | 全部 10 个 | 显式指定，不做探测收缩 |
| `minimal` | `status` + `devicelist` | 极老固件，连 `newstatus` 都没有 |
| `no-xqnetwork` | 4 个 `misystem/*` | `xqnetwork` 返回脏数据的固件 |
| `no-scan` | 默认集去掉 `wifi_list` | **推荐**，见下 |

### `no-scan`：一个由真机实测推导出来的 profile

`/api/xqnetwork/wifi_list` 会触发一次 WiFi 扫描（实测 13–14 秒），且
**扫描期间路由器阻塞其余全部 LuCI 请求**。在两台真实路由器上实测：

| 配置 | 抓取耗时 | 端点可用 |
|---|---|---|
| `default` | 5.0s（被超时钉死） | 1/10 |
| `no-scan` | 1–3s | 9/9 |

代价只有 `neighbor_wifi_rssi` / `neighbor_wifi_count` 这两个指标，
换来的是其余九个指标从「全挂」变成「全通」。默认值仍是 `default`
（老版本升级后指标不该凭空消失），但推荐生产用 `no-scan`。

### 为什么没有「慢端点专用超时表」

`baseProfile` 里有 `TimeoutFor` 这套机制，实测下来**默认是空的**，而且
代码里明确写了不要随便往里加。曾经的教训：

按「实测 13s 就配 20s 专用超时」的思路给 `wifi_list` 放宽到 20s，结果**更糟**——
扫描期间路由器阻塞其余请求，九个 0.2s 就返回的端点全被拖到超时，
整轮抓取从 5s 变成全面失败。**「慢的代价由别人承担」时，放宽超时是有害的。**

用户确有需要时用 `ENDPOINT_TIMEOUTS=wifi_list=20s` 显式打开，并自行承担代价。

### 为什么超时不能作为停问依据

`Instance.RecordResult` 会统计连续超时，但**永远不会**据此停问端点。

超时无法区分两件本质不同的事：

- 这个端点坏了
- 路由器被另一个慢端点堵住了（真机上就是 `wifi_list` 阻塞全场）

曾实现过「连续 3 次超时就停问」的启发式，实测 8 次抓取里把 **7 个健康端点
连带误杀**（含确定可用的 `status` / `lan_info` / `device_list`）。

停问只认 HTTP 404/501 —— 那是一次就确定、不会误伤的事实。


| 名字 | 端点 | 适用 |
|---|---|---|
| `auto`（默认） | 全部 10 个，逐个探测收敛 | 新旧通吃 |
| `default` | 全部 10 个 | 显式指定，不做探测收缩 |
| `minimal` | `status` + `devicelist` | 极老固件，连 `newstatus` 都没有 |
| `no-xqnetwork` | 4 个 `misystem/*` | `xqnetwork` 返回脏数据的固件 |

```bash
ROUTER_PROFILE=minimal ./miwifi-exporter
```

## 自适应探测

`auto` 模式下，如果某个端点返回 **HTTP 404 / 501**，会被判定为「这台机器上
压根没有这个接口」并停止请求它。

```
第 1 轮：wifi_list → 404 → 标记为不支持，日志「该固件不提供此端点，已停止采集」
第 2 轮起：不再请求 wifi_list，无 WARN，无恒零指标
```

这样一来，「某个指标恒为 0」就有了明确解释——看
`miwifi_exporter_endpoint_supported{endpoint="…"}` 就能分清是**坏了**还是
**这台机器本来就没这接口**。

被禁用的端点会在 **30 分钟**后重新探测一次。没有这一步的话，自适应降级会
退化成自适应遗忘：固件升级后新长出来的接口永远采不到。

## 怎么加一个新固件

### 情况 A：新固件只是少了几个接口

在 `builtin.go` 加一个 profile：

```go
func init() {
    builtins := []Profile{
        // …现有内置…
        newBase(
            "ax6000-legacy",              // 名字，会出现在 ROUTER_PROFILE 与日志里
            "AX6000 早期固件：缺 xqnetwork 系���",
            endpointSet(EPStatus, EPDeviceList, EPNewStatus),
            dualBands,                    // 双频段映射
        ),
    }
    // …
}
```

### 情况 B：新固件登录页结构变了

在 `login.go` 加一个 parser：

```go
func NewMyFirmwareParser() *LuCIParser {
    return &LuCIParser{
        keyRe:    regexp.MustCompile(`"authKey":\s*"(.*?)"`),
        deviceRe: regexp.MustCompile(`var devId = "(.*?)"`),
        descripton: "AX6000 新固件登录页",
    }
}
```

然后在 `builtin.go` 里把该 profile 的 `parser` 覆盖掉。

> **不要就地修改 `DefaultLuCIParser` 的正则。** 直接改正则会让
> 「哪些型号用哪个 parser」这条信息丢失；标准 parser 应当保持对标准页面
> 仍然可用，这样两套规则才能共存、按型号选择。

### 情况 C：新固件引入了新的散列模式

`init_info` 里的 `newEncryptMode` 取值变了。在 `builtin.go` 的 `Hasher` 里
加一条 case：

```go
case 2:
    return sha512Hasher, nil
```

未识别的取值会**明确报错**而不是悄悄回落到 SHA1——宁可登录失败让人看见，
也不要拿着一把错误的钥匙反复撞门。

### 情况 D：完全自定义

实现 `profile.Profile` 接口，然后注册：

```go
func init() {
    if err := profile.Register(&myFirmware{}); err != nil {
        panic(err)
    }
}
```

注册表是进程全局的；同名注册会被拒绝（静默覆盖会让「我到底选中了哪个
profile」变得无法排查）。

## 检查当前状态

```bash
# 启动日志会打印选中的 profile 与实际会请求的端点数
ROUTER_LOG_LEVEL=debug ./miwifi-exporter
# 固件 profile 已选定 profile=default endpoints=全部 10 个端点

# 哪些端点被跳过了、为什么
curl -s localhost:9001/metrics | grep miwifi_exporter_endpoint_supported
```

| 指标 | 含义 |
|---|---|
| `miwifi_exporter_endpoint_supported{endpoint}` | 1=会采集，0=已跳过 |
| `miwifi_exporter_endpoint_up{endpoint}` | 本轮该端点是否取数成功 |
| `miwifi_exporter_endpoint_stale{endpoint}` | 当前值是否为历史遗留 |

## 测试

```bash
go test ./internal/profile/          # profile 自身
go test ./internal/router/ -run Profile  # 与客户端的集成（跳过不支持的端点）
go test ./internal/router/ -run Adaptive # 自适应降级与复探
```

`login_test` 里 `TestCustomParserIsPlugable` 演示了「新固件加 parser」这条路
怎么走：自定义 parser 在新页面上成功，同时**标准 parser 在同一页面上失败**
——这正是两套规则必须共存的理由。
