# 多阶段构建：golang 编译 → alpine 运行时（含 adb 客户端）
#
# 网络桥接方案：容器内 adb 通过 ADB_SERVER_SOCKET 指向宿主机 adb server（USB 设备），
# 无显式依赖（每次操作 adbmgr 自动重连），容器重启不丢设备状态。
FROM golang:1.26-alpine AS build
WORKDIR /src
# 国内网络：模块代理镜像（proxy.golang.org 直连超时）
ENV GOPROXY=https://goproxy.cn,direct
# curl+bash: go generate 下载 cloudflared 二进制（//go:embed 需要）
# APK 国内镜像：官方 dl-cdn.alpinelinux.org 国内拉包极慢（实测 70 包卡 1 分钟+）
RUN sed -i 's#https://dl-cdn.alpinelinux.org#https://mirrors.tuna.tsinghua.edu.cn#g' /etc/apk/repositories && apk add --no-cache curl bash
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY web web
COPY scripts scripts
# 下载 cloudflared 并嵌入二进制（用于 CF Tunnel 回调地址）
RUN bash scripts/download-cloudflared.sh
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/gateway ./cmd/gateway

FROM alpine:3.20
# adb 客户端 + CA 证书（云通道/回调 HTTPS）
RUN sed -i 's#https://dl-cdn.alpinelinux.org#https://mirrors.tuna.tsinghua.edu.cn#g' /etc/apk/repositories && \
    apk add --no-cache android-tools ca-certificates tzdata && \
    addgroup -S pgw && adduser -S pgw -G pgw
COPY --from=build /out/gateway /app/gateway
WORKDIR /app
# 数据/日志/配置挂载点：sqlite（data/3 个库）、lumberjack 日志、.env 敏感配置
VOLUME ["/app/data", "/app/logs"]
ENV GATEWAY_ADDR=":8080" \
    ADMIN_ADDR=":8081" \
    ORDER_DB="data/paygateway.db" \
    LOG_FILE="logs/paygateway-{date}.log" \
    TZ="Asia/Shanghai"
USER pgw
EXPOSE 8080 8081
ENTRYPOINT ["/app/gateway"]
