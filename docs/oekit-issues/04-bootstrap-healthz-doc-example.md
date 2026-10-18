## 问题

`healthz` 包文档里的集成示例签名是错的。`bootstrap@v0.6.0/healthz/healthz.go` 第 17-18 行：

```go
h.GET("/healthz", func(_ context.Context, c *app.RequestContext) {
	status, body := healthz.Handle(c)   // ← 传的是 c
	c.JSON(status, body)                 // ← body 是 string
})
```

实际签名：

```go
func Handle(ctx context.Context) (int, string)
```

两处都错，而且**都很隐蔽**：

1. **传参**：`app.RequestContext` 恰好实现了 `context.Context`，所以 `healthz.Handle(c)` **能编译通过**——但传进去的是一个 RequestContext，`Handle` 内部会用它做 `context.WithTimeout(ctx, timeout)`，语义上完全不是调用方想要的。
2. **返回值**：`body` 是 `string` 而非结构体，`c.JSON(status, body)` 会把一个已经序列化好的 JSON 字符串**再序列化一次**，得到 `"{\"status\":\"ok\"}"` 这种双重编码的载荷——k8s 探针只看状态码能过，但任何按 JSON 解析的探针消费者（Prometheus blackbox、自研监控）都会炸。

## 正确写法

```go
h.GET("/healthz", func(ctx context.Context, c *app.RequestContext) {
	status, body := healthz.Handle(ctx)
	c.Data(status, "application/json; charset=utf-8", []byte(body))
})
```

## 复现

```bash
go doc github.com/oekit/bootstrap/healthz.Handle
sed -n '10,22p' $(go env GOMODCACHE)/github.com/oekit/bootstrap@v0.6.0/healthz/healthz.go
```

## 补充建议

`Handle` 返回 `(int, string)` 让每个调用方都要自己决定 content-type 和字节转换。可以考虑补一个 Hertz 适配导出，省掉每个使用方都抄一遍这三行：

```go
func HTTPMiddleware(paths ...string) app.HandlerFunc   // 内建 /healthz /livez /readyz 三条路由
```

（`/livez` 恒 200、`/readyz` 含全部 check 这个约定目前只写在包注释里，没有代码兜底，容易被漏实现。）

---
*提交自一次真实的全量重构（把一个 Prometheus exporter 整体迁到 oekit 之上）。文中数据均为实测，可按给出的命令复现。*