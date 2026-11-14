# 部署与运维资产

这个目录放的是**不在二进制里**、但线上要用的东西：

| 文件 | 作用 | 怎么用 |
|---|---|---|
| `prometheus/miwifi-alerts.yaml` | Prometheus 告警规则（7 条） | 见下 |
| `grafana/miwifi-dashboard.json` | Grafana dashboard（16 个面板） | Grafana → Dashboards → Import → Upload JSON |

## Prometheus 告警规则

规则全部基于 exporter 自带的自监控指标，**不需要额外的 exporter 或 sidecar**。

原生 Prometheus（`rule_files`）：

```yaml
# /etc/prometheus/prometheus.yml
rule_files:
  - /etc/prometheus/rules/miwifi-alerts.yaml
```

```bash
cp deploy/prometheus/miwifi-alerts.yaml /etc/prometheus/rules/
curl -X POST http://127.0.0.1:9090/-/reload
# 或 systemctl reload prometheus
```

Prometheus Operator（`PrometheusRule` CRD）：

```bash
kubectl apply -f deploy/prometheus/miwifi-alerts.yaml
```

> **job 名**：规则里用 `job=~"miwifi.*"` 匹配 exporter 自身的抓取任务。
> 如果你的 job 名不是这个形状，改这一条即可，其余规则只用 exporter 导出的指标。

### 规则清单

| 告警 | 触发条件 | 级别 | 含义 |
|---|---|---|---|
| `MiwifiExporterDown` | `up == 0` 持续 3m | critical | **采集机本身挂了**（最该有却一直缺的一条） |
| `MiwifiRouterUnreachable` | `router_up == 0` 持续 5m | critical | 路由器采不到数据 |
| `MiwifiSnapshotStale` | `snapshot_age_seconds > 90` 持续 5m | warning | poll 模式后台采集卡住 |
| `MiwifiEndpointRepeatedFailure` | 端点因连续失败被停采 | warning | 端点真的坏了 |
| `MiwifiEndpointFailing` | 10 分钟内失败 ≥3 次 | warning | 端点反复失败 |
| `MiwifiEndpointServingStaleValue` | 导出陈旧值持续 15m | warning | 数字很久没更新了 |
| `MiwifiCollectRepeatedFailure` | 15 分钟内出现整轮失败 | warning | 整轮采集失败 |

### 为什么「固件不支持」不算告警

`miwifi_exporter_endpoint_disabled` 带一个 `reason` 标签：

| reason | 含义 | 该不该告警 |
|---|---|---|
| `not_supported` | 固件压根不提供这个接口 | 不该（正常） |
| `repeated_failure` | 连续多轮返回相同错误 | 该 |
| `config` | 配置显式排除 | 不该（人为决定） |
| 空 | 未停采 | 不该 |

所以规则里只对 `repeated_failure` 报警，另两类要么是固件事实、要么是用户的明确意志。

## Grafana dashboard

```bash
# Grafana UI：Dashboards → New → Import → Upload JSON file
# 选择 deploy/grafana/miwifi-dashboard.json
```

或者用 API 导入：

```bash
curl -X POST http://127.0.0.1:3000/api/dashboards/db \
  -H 'Content-Type: application/json' \
  -u admin:admin \
  -d "$(python3 - <<'PY'
import json
d = json.load(open("deploy/grafana/miwifi-dashboard.json"))
d.pop("id", None)
print(json.dumps({"dashboard": d, "overwrite": True}))
PY
)"
```

导入后到 dashboard 设置里选 Prometheus 数据源（`DS_PROMETHEUS` 变量）。

## 排查入口

exporter 自带两个 HTTP 端点，排查时先看它们：

```bash
curl -s http://<采集机>:9001/routers | python3 -m json.tool
#   每台的 profile / 启用的端点 / 被停采的端点 / 本轮失败的端点及原因 / 端点集合差异

curl -s http://<采集机>:9001/healthz
#   逐台健康快照

curl -s http://<采集机>:9001/metrics | grep '^miwifi_exporter_'
#   自监控指标全套
```
