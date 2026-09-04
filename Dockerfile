# syntax=docker/dockerfile:1.6
# AIStudio2API HuggingFace Spaces Docker 镜像
#
# 设计:
#   - 多阶段构建,build stage 用 Go 1.26 + Node 24 编译前后端
#   - runtime stage 用 Ubuntu 22.04,安装 Firefox/Camoufox 运行时依赖
#   - Camoufox 在 build stage 预下载到镜像,避免 HF 网络不稳定
#   - 单账户在线: WARM_WORKER_LIMIT=1 / MAX_ACTIVE_WORKERS=1
#   - 监听 0.0.0.0:7860(HF Spaces 强制端口)
#   - 多账户凭证通过 AISTUDIO_AUTH_ACCOUNTS 环境变量注入

# ============================== build stage ==============================
FROM golang:1.26-bookworm AS builder

# 安装 Node.js 24(用于构建 Vue 前端)
RUN curl -fsSL https://deb.nodesource.com/setup_24.x | bash - && \
    apt-get install -y --no-install-recommends nodejs git ca-certificates unzip && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /src

# 先复制模块文件利用缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 构建前端 -> 输出到 internal/webui/dist
RUN cd web && npm ci && npm run build

# 构建后端二进制
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/aistudio2api ./cmd/aistudio2api

# ============================ camoufox stage ============================
FROM ubuntu:22.04 AS camoufox

# 从源码动态读取 Camoufox 版本,避免与代码常量不同步
# 内部/camoufoxnative/download.go 中: const camoufoxRelease = "152.0.4-beta.29"
COPY internal/camoufoxnative/download.go /tmp/download.go
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl unzip grep && \
    rm -rf /var/lib/apt/lists/* && \
    CAMOUFOX_VERSION=$(grep -oE 'camoufoxRelease = "[^"]+"' /tmp/download.go | head -1 | sed -E 's/.*"([^"]+)".*/\1/') && \
    test -n "$CAMOUFOX_VERSION" && \
    echo "Camoufox version: $CAMOUFOX_VERSION" && \
    curl -fsSL --retry 5 --retry-delay 10 --retry-all-errors --progress-bar \
      -o /tmp/camoufox.zip \
      "https://github.com/daijro/camoufox/releases/download/v${CAMOUFOX_VERSION}/camoufox-${CAMOUFOX_VERSION}-lin.x86_64.zip" && \
    mkdir -p /camoufox && \
    unzip -q /tmp/camoufox.zip -d /camoufox && \
    rm /tmp/camoufox.zip /tmp/download.go && \
    chmod +x /camoufox/camoufox-bin && \
    test -x /camoufox/camoufox-bin

# ============================ runtime stage ============================
FROM ubuntu:22.04 AS runtime

# Firefox/Camoufox 运行时依赖(GTK3, X11, NSS, ALSA, dbus-glib, 字体, curl for healthcheck)
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      tzdata \
      fonts-liberation fonts-noto-color-emoji \
      libgtk-3-0 libgtk-3-bin \
      libasound2 libdbus-glib-1-2 \
      libx11-6 libx11-xcb1 \
      libxcb1 libxcb-shm0 \
      libxcomposite1 libxcursor1 libxdamage1 \
      libxext6 libxfixes3 libxi6 \
      libxrandr2 libxrender1 libxss1 \
      libxtst6 libxinerama1 \
      libpango-1.0-0 libpangocairo-1.0-0 \
      libcairo2 libatk1.0-0 libatk-bridge2.0-0 \
      libdrm2 libgbm1 libglib2.0-0 \
      libnss3 libnspr4 \
      libstdc++6 \
      dbus-x11 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# 拷贝二进制和 Camoufox
COPY --from=builder /out/aistudio2api /app/aistudio2api
COPY --from=camoufox /camoufox /app/runtime/camoufox

# 运行时目录
RUN mkdir -p /app/auth && \
    chmod +x /app/aistudio2api /app/runtime/camoufox/camoufox-bin

# HuggingFace Spaces 强制 7860 端口
EXPOSE 7860

# 默认环境变量(可被 HF Spaces Variables 覆盖)
ENV LISTEN_ADDR=0.0.0.0:7860 \
    AISTUDIO_AUTH_STATES=/app/auth \
    CAMOUFOX_PATH=/app/runtime/camoufox/camoufox-bin \
    PROXY_API_KEY= \
    PROXY= \
    INIT_TIMEOUT=3m \
    REQUEST_TIMEOUT=5m \
    WARM_WORKER_LIMIT=1 \
    MAX_ACTIVE_WORKERS=1 \
    WARM_STARTUP_CONCURRENCY=1 \
    PER_ACCOUNT_CONCURRENCY=2 \
    TEMPORARY_CHAT=true \
    AISTUDIO_FAILOVER=true \
    TZ=Asia/Shanghai \
    LANG=en_US.UTF-8 \
    LC_ALL=en_US.UTF-8

# 管理端鉴权 token(HF Spaces 必填,通过 Secrets 注入)
# 不设置则管理端仅允许 loopback(HF 反代不可访问,功能受限)
# ADMIN_TOKEN=my_admin_secret

# 管理界面在浏览器自动打开在 HF 无意义,关闭
CMD ["/app/aistudio2api", "-open-ui=false"]

# 健康检查: /health 在服务未就绪时返回 503(HF 据此决定是否路由流量)
HEALTHCHECK --interval=30s --timeout=10s --start-period=120s --retries=3 \
    CMD curl -fsS http://127.0.0.1:7860/health || exit 1
