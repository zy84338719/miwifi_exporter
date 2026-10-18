# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.26-alpine AS builder

WORKDIR /src

# 依赖层单独缓存：只要 go.mod/go.sum 没变就不必重新下载。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/miwifi-exporter .

# ---- runtime ----
FROM alpine:3.20

# ca-certificates：路由器走 https 时需要；tzdata：日志时间戳本地化。
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 exporter

WORKDIR /app
COPY --from=builder /out/miwifi-exporter /app/miwifi-exporter

USER exporter
EXPOSE 9001

ENV ROUTER_PORT=9001 \
    METRICS_PATH=/metrics \
    METRICS_NAMESPACE=miwifi \
    COLLECT_MODE=pull \
    LOG_LEVEL=info

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- "http://127.0.0.1:${ROUTER_PORT}/livez" >/dev/null || exit 1

ENTRYPOINT ["/app/miwifi-exporter"]
