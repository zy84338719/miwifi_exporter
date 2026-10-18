# 本地构建用的最小 Dockerfile：复用已在树莓派上交叉编译好的 arm64 二进制。
# 与仓库根目录的 Dockerfile 运行时层保持一致（alpine + ca-certificates + tzdata），
# 差别只在于不做多阶段编译——Pi 3 上没有 Go，也不该装。
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 exporter

WORKDIR /app
COPY miwifi-exporter /app/miwifi-exporter

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
