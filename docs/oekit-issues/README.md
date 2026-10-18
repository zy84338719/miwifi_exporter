# 给 oekit 提交的改进需求

把 `miwifi-exporter` 整体重构到 oekit 之上时踩到的坑，逐条核实后提交。

正文即 issue 原文，与 GitHub 上的完全一致（提交时间 2026-10-18）。

| 文件 | 仓库 | Issue |
|---|---|---|
| `01-matrix-tools-versions-stale.md` | oekit/matrix-tools | [#5](https://github.com/oekit/matrix-tools/issues/5) |
| `02-site-llms-middleware-path.md` | oekit/oekit.github.io | [#9](https://github.com/oekit/oekit.github.io/issues/9) |
| `03-middleware-default-stack-skip.md` | oekit/middleware | [#15](https://github.com/oekit/middleware/issues/15) |
| `04-bootstrap-healthz-doc-example.md` | oekit/bootstrap | [#4](https://github.com/oekit/bootstrap/issues/4) |
| `05-bootstrap-healthz-registry-singleton.md` | oekit/bootstrap | [#5](https://github.com/oekit/bootstrap/issues/5) |
| `06-hertz-newlogger-doc-accuracy.md` | oekit/hertz | [#10](https://github.com/oekit/hertz/issues/10) |
| `07-logx-parseloglevel.md` | oekit/logx | [#13](https://github.com/oekit/logx/issues/13) |
| `08-concurrency-singleflight-shared.md` | oekit/concurrency | [#18](https://github.com/oekit/concurrency/issues/18) |
| `09-core-config-env-over-file.md` | oekit/core | [#2](https://github.com/oekit/core/issues/2) |

另有一条候选**未提交**：`pkg/errors` 缺 `New`——oekit/pkg 已有 open issue #8 覆盖，未重复提。

## 未被采纳的两条自省

写这份清单时有两条初稿被自己否掉了，因为核实后发现是**我的错**而不是 oekit 的：

- 「`httpjson` 无法按状态码分类」——实际 `StatusError.Code` 字段已存在，`RetryConfig.Retryable` 钩子也有，完全够用。是我的使用场景需要更细的错误语义（403 = stok 失效需重登、404 = 端点不存在需停探），不属于 httpjson 的职责范围。
- 「`hertz` 与 `middleware` 的日志桥接文档互相矛盾」——实际 middleware 的 README 根本没提 `WithService`，是我把两段不相干的文档记混了。
