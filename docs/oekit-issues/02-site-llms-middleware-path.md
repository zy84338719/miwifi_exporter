## 问题

`docs/public/llms.txt` 第 23 行对 middleware 的路径描述与实际包布局不符：

```
- [middleware](https://github.com/oekit/middleware): Hertz 标准中间件栈（包名 mw，路径 `middleware/middleware` 双层目录）+ contrib 桥接 + hmw（`middleware/traceid/hmw` → 模块根 `middleware/hmw`）
```

实际情况（middleware@v0.7.0）：包在**模块根**，包名 `mw`，即 `github.com/oekit/middleware`。

- `middleware/middleware` 这个路径**不存在**
- `middleware/traceid/hmw` 已挪到模块根 `middleware/hmw`，这半句是对的

## 影响

`llms.txt` 是 AI agent 的主入口文档，这一条会直接导致写错 import。本次重构第一版就是这么写的，编译报错才发现：

```go
middleware.RecoveryMiddleware()   // ❌ undefined: middleware
```

实际应为：

```go
mw "github.com/oekit/middleware"
mw.RecoveryMiddleware()
```

## 与 oekit.github.io#6 的关系

#6（已关闭）覆盖过同一批路径映射问题，其中也包含 middleware 的双层目录表述。本次核对时 `llms.txt` 里的**该段文字仍在**，所以要么 #6 的修复没落到这一句，要么后来被覆盖回来了。

## 复现

```bash
curl -s https://oekit.github.io/llms.txt | grep -n middleware | head -3
# 实际布局：
go doc github.com/oekit/middleware   # → package mw
ls $(go env GOMODCACHE)/github.com/oekit/middleware@v0.7.0/  # 无 middleware/ 子目录
```

## 建议

改为：

> `middleware`](https://github.com/oekit/middleware): Hertz 标准中间件栈（**包名 `mw`，模块根包 `github.com/oekit/middleware`，需 import 别名 `mw`**）+ contrib 桥接 + hmw（`middleware/hmw`）

补充说明：`middleware/traceid/hmw` 已于目录收平（v0.5.1）时挪到模块根 `middleware/hmw`，旧副本已删除（另见 oekit/middleware#11）。

---
*提交自一次真实的全量重构（把一个 Prometheus exporter 整体迁到 oekit 之上）。文中数据均为实测，可按给出的命令复现。*