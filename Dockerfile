# syntax=docker/dockerfile:1.6
# AIStudio2API HuggingFace Spaces Docker 镜像
#
# 设计:
#   - 四阶段构建:
#       frontend  node:24 官方镜像构建 Vue 前端(替代已弃用的 NodeSource 脚本)
#       builder   golang:1.26 编译后端,go mod 与 npm ci 分层缓存
#       camoufox  预下载 Camoufox 到镜像,避免 HF 网络不稳定
#       runtime   ubuntu 22.04 + Firefox/Camoufox 运行时依赖
#   - HF Spaces 强制以 UID 1000 运行容器(官方文档),镜像必须预先
#     以 1000 属主准备好可写目录,否则 /app/auth 写入直接 permission denied
#   - Camoufox 以非 root 运行: root 身份跑浏览器会暴露大量自动化特征
#   - 监听 0.0.0.0:7860(HF Spaces 强制端口)
#   - 多账户凭证通过 AISTUDIO_AUTH_ACCOUNTS 环境变量注入

# ============================== frontend stage ==============================
# 使用官方 Node 镜像而非 NodeSource curl|bash 脚本(NodeSource 旧式
# setup_*.x 脚本已弃用且依赖第三方网络),官方镜像更快更可靠
FROM node:24-bookworm-slim AS frontend

WORKDIR /src

# 先复制依赖清单,仅当 lockfile 变化时才重新 npm ci(层缓存)
COPY web/package.json web/package-lock.json ./web/
RUN cd web && npm ci

# 复制前端源码并构建,vite 按 outDir 输出到 /src/internal/webui/dist
COPY web/ ./web/
RUN cd web && npm run build && test -f /src/internal/webui/dist/index.html

# =============================== builder stage ===============================
FROM golang:1.26-bookworm AS builder

# BuildKit 自动注入 TARGETARCH(与目标平台一致);
# 经典构建器不注入时回退到基础镜像自身架构,保证本地 docker build
# 在任何宿主机上都能产出与基础镜像架构一致的二进制
ARG TARGETARCH

WORKDIR /src

# 先复制模块文件,仅当 go.mod/go.sum 变化时才重新下载依赖(层缓存)
COPY go.mod go.sum ./
RUN go mod download

# 复制源码与前端产物(前端产物由 frontend stage 生成)
COPY . .
COPY --from=frontend /src/internal/webui/dist ./internal/webui/dist

# 构建后端二进制(纯静态,CGO 关闭;modernc.org/sqlite 为纯 Go 实现)
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-$(dpkg --print-architecture)} \
    go build -trimpath -ldflags="-s -w" -o /out/aistudio2api ./cmd/aistudio2api

# ============================= camoufox stage =============================
FROM ubuntu:22.04 AS camoufox

# 架构护栏: Camoufox 官方仅发布 linux x86_64 构建,在非 amd64 平台
# 构建时快速失败并给出明确错误,而不是产出二进制与基础镜像架构
# 不符的静默损坏镜像(例如 Apple Silicon 上本地 docker build)
ARG TARGETARCH

# 从源码动态读取 Camoufox 版本,避免与代码常量不同步
# 内部/camoufoxnative/download.go 中: const camoufoxRelease = "152.0.4-beta.29"
COPY internal/camoufoxnative/download.go /tmp/download.go
RUN BUILD_ARCH="${TARGETARCH:-$(dpkg --print-architecture)}" && \
    if [ "$BUILD_ARCH" != "amd64" ]; then \
        echo "ERROR: 暂不支持 linux/$BUILD_ARCH——Camoufox 官方未发布 linux aarch64 构建,请使用 --platform linux/amd64" >&2; \
        exit 1; \
    fi && \
    apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl unzip grep && \
    rm -rf /var/lib/apt/lists/* && \
    CAMOUFOX_VERSION=$(grep -oE 'camoufoxRelease = "[^"]+"' /tmp/download.go | head -1 | sed -E 's/.*"([^"]+)".*/\1/') && \
    test -n "$CAMOUFOX_VERSION" && \
    echo "Camoufox version: $CAMOUFOX_VERSION" && \
    curl -fsSL --retry 5 --retry-delay 10 --retry-all-errors \
      --silent --show-error \
      -o /tmp/camoufox.zip \
      "https://github.com/daijro/camoufox/releases/download/v${CAMOUFOX_VERSION}/camoufox-${CAMOUFOX_VERSION}-lin.x86_64.zip" && \
    echo "Camoufox ${CAMOUFOX_VERSION} 下载完成 $(du -h /tmp/camoufox.zip | cut -f1)" && \
    mkdir -p /camoufox && \
    unzip -q /tmp/camoufox.zip -d /camoufox && \
    rm /tmp/camoufox.zip /tmp/download.go && \
    chmod +x /camoufox/camoufox-bin && \
    test -x /camoufox/camoufox-bin

# ============================= runtime stage =============================
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
      libxtst6 libxinerama1 libxt6 \
      libpango-1.0-0 libpangocairo-1.0-0 \
      libcairo2 libatk1.0-0 libatk-bridge2.0-0 \
      libdrm2 libgbm1 libglib2.0-0 \
      libnss3 libnspr4 \
      libstdc++6 \
      dbus-x11 \
    && rm -rf /var/lib/apt/lists/*

# 非 root 运行用户,UID 固定 1000:
#   - HF Spaces 无论 Dockerfile 如何声明都以 UID 1000 运行容器,
#     显式创建同名用户可保证本地 docker run 与 HF 行为一致
#   - Camoufox/Firefox 以 root 运行会暴露自动化特征(/proc/self、
#     HOME 属主等可被页面 JS 检测),非 root 是反指纹的基本要求
RUN useradd --uid 1000 --user-group --create-home --shell /usr/sbin/nologin appuser

WORKDIR /app

# 拷贝二进制与 Camoufox
# 注意: --chown 自 Docker 17.09 起被经典构建器支持;但 --chmod 是 BuildKit
# 专属特性,为确保 HF Spaces 构建器兼容性,可执行位用 RUN chmod 设置
COPY --from=builder /out/aistudio2api /app/aistudio2api
COPY --from=camoufox --chown=1000:1000 /camoufox /app/runtime/camoufox
RUN chmod +x /app/aistudio2api /app/runtime/camoufox/camoufox-bin

# 运行时可写目录:账户配置/凭证(runtime-state、.leases 均在 auth 下);
# runtime 授权保证 CAMOUFOX_PATH 失效时的自动安装回退路径可写;
# Camoufox profile 使用 /tmp(粘滞位,任何用户可写),无需额外授权
RUN mkdir -p /app/auth /app/runtime && chown -R 1000:1000 /app/auth /app/runtime

USER appuser
ENV HOME=/home/appuser

# HuggingFace Spaces 强制 7860 端口
EXPOSE 7860

# 默认环境变量(可被 HF Spaces Variables 覆盖)
# GOMEMLIMIT 为 Go 运行时软内存上限,可在 HF Variables 按需覆盖(如 8GiB)
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
# start-period 180s: HF 网络慢 + Camoufox 首次预热可能超 120s,给足余量
HEALTHCHECK --interval=30s --timeout=10s --start-period=180s --retries=3 \
    CMD curl -fsS http://127.0.0.1:7860/health || exit 1
