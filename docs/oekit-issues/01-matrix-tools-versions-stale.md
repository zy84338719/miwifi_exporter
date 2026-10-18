## 背景

`versions.json` 的 12 个模块里 **8 个版本号是过期的**，而文件的 `generatedAt` 显示是**当天生成**的（`2026-10-17T13:20:35Z`）。

这比「不更新」更糟：`versions.json` 是 `llms.txt` 指定的机器可读版本矩阵，agent 会把它当成权威快照照单钉版本；一个「看起来很新但内容是旧的」文件，比一个明显陈旧的文件危害大得多。

## 实测（2026-10-17）

对照 `https://proxy.golang.org/github.com/oekit/<mod>/@latest`：

| 模块 | versions.json 声称 | 实际 @latest |
|---|---|---|
| logx | v0.5.0 | **v0.6.0** |
| core | v0.6.0 | **v0.7.0** |
| concurrency | v0.5.4 | **v0.6.0** |
| bootstrap | v0.5.4 | **v0.6.1** |
| middleware | v0.6.2 | **v0.7.1** |
| hertz | v0.5.10 | **v0.6.0** |
| httpx | v0.3.2 | **v0.4.1** |
| registry | v0.4.0 | **v0.4.1** |
| pkg / data / timertask / gormx / network | — | 一致 |

复现：

```bash
curl -s https://oekit.github.io/versions.json | jq -r '.generatedAt, (.modules[] | "\(.module) \(.latest)")'
for m in logx core concurrency bootstrap middleware hertz httpx registry; do
  printf "%-12s " "$m"
  curl -s "https://proxy.golang.org/github.com/oekit/$m/@latest" | jq -r .Version
done
```

## 根因（读 gen_versions.py 后定位）

`scripts/gen_versions.py` 的取数全部走**本地 git clone**：

```python
def _git_tags(d: Path) -> list[str]:
    out = subprocess.check_output(["git", "-C", str(d), "tag", "-l", "v*", "--sort=-v:refname"], ...)

def latest_version(repo: str) -> str | None:
    tags = _git_tags(repo_dir(repo, BASE))
```

而 `generatedAt` 是**无条件**取的当前时间：

```python
return {
    "generatedAt": datetime.utcnow().strftime("%Y-%m-%dT%H:%M:%SZ"),
    ...
}
```

于是：只要本地 clone 没有 `git fetch --tags`，生成器就会输出一份**内容陈旧但时间戳全新**的数据，没有任何告警。

## 建议

1. **生成前校验新鲜度**：对每个仓比对本地 `git ls-remote --tags origin` 与本地 tag，不一致就打 `[warn] <repo>: 本地 tag 落后，远端更新到 vX.Y.Z`；`--strict` / CI 模式下直接 fail。
2. **`generatedAt` 改为反映数据本身**，例如取所有参与模块中最旧的 tag 提交时间，或额外输出 `dataAsOf`（最旧 tag 的日期）与 `generatedAt`（本次运行时间）两个字段。
3. **CI 巡检**：重跑生成器后与已发布文件 diff，非空则 fail——和 oekit.github.io#7 里提到的自动化诉求一致，这里补的是「怎么保证它真的刷新了」。
4. 顺手：`build()` 里 `repo_dir` 不存在时 `continue` 会让该模块**静默消失**（不是版本空，是整条不见）。建议保留条目并把 `latest` 置为 `null` + 一个 `error` 字段，让缺失可见。

---
*提交自一次真实的全量重构（把一个 Prometheus exporter 整体迁到 oekit 之上）。文中数据均为实测，可按给出的命令复现。*