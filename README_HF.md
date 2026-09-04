# HuggingFace Spaces 部署指南

本文档说明如何将 AIStudio2API 部署到 HuggingFace Spaces Docker 环境，支持多账号环境变量注入、单账号在线、随时切换、自动 failover。

---

## 一、整体设计

| 维度 | 设计 |
|------|------|
| 凭证注入 | 通过环境变量 `AISTUDIO_AUTH_ACCOUNTS` 注入多账号 JSON，支持 `base64:` / `gzip:` 前缀（gzip 应对 HF 64KB 限制） |
| 多账号 | 启动时把所有账号写入 `/app/auth/<email>/`，AccountStore 自动加载 |
| 单账号在线 | 默认仅 `AISTUDIO_ACTIVE_EMAIL`（或第一个）账号 `Enabled=true`，其余 `Enabled=false` |
| 账号切换 | 复用 Web 管理界面"账户"页：停用当前账号 → 启用目标账号，无需重启；或通过 `PUT /api/accounts/{id}` API |
| 自动 failover | `AISTUDIO_FAILOVER=true` 时后台监控 active 账号失效，自动切换到备用账号（失效账号拉黑防震荡） |
| 管理端鉴权 | `ADMIN_TOKEN` 非空时启用 token 鉴权（HF 必填，否则管理操作被 loopback 限制 403） |
| 监听端口 | `0.0.0.0:7860`（HF Spaces 强制端口） |
| 健康检查 | `/health` 在服务未就绪时返回 503，避免 HF 提前路由流量导致 502 |
| 并发约束 | `WARM_WORKER_LIMIT=1`、`MAX_ACTIVE_WORKERS=1`、`WARM_STARTUP_CONCURRENCY=1` |
| Camoufox | Docker 镜像中预下载，版本号从源码动态读取，避免与代码常量不同步 |

---

## 二、获取 storage-state.json

AIStudio2API 通过 Playwright `storage-state.json` 持有 Google 登录态。**你必须在本地登录一次拿到这个文件**，再上传到 HF Spaces。

### 方式 A：隔离 Camoufox 登录（推荐，跨平台）

```bash
# 本地需要 Go 1.26+ 和 Node.js 24+
git clone https://github.com/zhu748/AIStudio2API.git
cd AIStudio2API
cp .env.example .env

# 构建前端
cd web && npm ci && npm run build && cd ..

# 构建后端
go build -o aistudio2api ./cmd/aistudio2api

# 启动隔离登录(会弹出 Camoufox 窗口,完成 Google 登录)
./aistudio2api setup --login
```

登录完成后，凭证保存在 `auth/<邮箱>/storage-state.json`。

### 方式 B：直接使用现有 storage-state.json

如果你已通过其他方式（Playwright/手工导出）拿到 `storage-state.json`，可直接用：

```bash
./aistudio2api setup --storage-state /path/to/storage-state.json
```

### 关键 Cookie

`storage-state.json` 必须包含以下 Google 认证 Cookie，否则启动会被 Signer 拒绝：

- `SAPISID`
- `__Secure-1PAPISID`
- `__Secure-3PAPISID`

---

## 三、构造 AISTUDIO_AUTH_ACCOUNTS 环境变量

### JSON 格式

```json
[
  {
    "email": "account1@gmail.com",
    "storage_state": {
      "cookies": [...],
      "origins": [...]
    },
    "proxy": "http://user1-residential:8080",
    "locale": "en-US",
    "timezone": "America/New_York"
  },
  {
    "email": "account2@gmail.com",
    "storage_state": {
      "cookies": [...],
      "origins": [...]
    },
    "proxy": "socks5://user2-residential:1080"
  }
]
```

可选字段（针对每个账号独立配置）：

| 字段 | 作用 | 缺省值 |
|------|------|--------|
| `proxy` | 该账号专用代理（HTTP/HTTPS/SOCKS5） | 取 `AISTUDIO_DEFAULT_PROXY` 或全局 `PROXY` |
| `locale` | 浏览器语言，影响请求头 `Accept-Language` | 取 `AISTUDIO_DEFAULT_LOCALE` 或系统 locale |
| `timezone` | 浏览器时区，影响页面行为 | 取 `AISTUDIO_DEFAULT_TIMEZONE` 或系统时区 |

> **代理优先级**：账号 JSON 内 `proxy` > `AISTUDIO_DEFAULT_PROXY` 环境变量 > 全局 `PROXY` 环境变量。
>
> **HF 部署推荐**：每个账号用不同住宅代理 IP，能显著降低 Google 风控触发率。

如果 `storage_state` 中已包含 `aistudio2api.source.email` 扩展字段（项目自己的 `setup --login` 产生的文件就有），可以省略外层 `email`：

```json
[
  { "cookies": [...], "origins": [...], "aistudio2api": {"source": {"email": "account1@gmail.com"}}, "proxy": "http://..." },
  { "cookies": [...], "origins": [...], "aistudio2api": {"source": {"email": "account2@gmail.com"}}, "proxy": "socks5://..." }
]
```

### 生成命令

```bash
# 假设你有两个 storage-state.json 文件
python3 -c "
import json
accounts = []
for path in ['/path/to/auth/account1@gmail.com/storage-state.json',
             '/path/to/auth/account2@gmail.com/storage-state.json']:
    state = json.load(open(path))
    email = path.split('/')[-2]
    accounts.append({'email': email, 'storage_state': state})
print(json.dumps(accounts))
" > /tmp/auth_accounts.json

# 检查大小（HF Secrets 单值上限约 64KB）
wc -c /tmp/auth_accounts.json
```

### 编码格式选择

| 编码 | 体积 | 适用场景 | 生成命令 |
|------|------|----------|----------|
| JSON 明文 | 基准 | 单账号或小数据 | `cat /tmp/auth_accounts.json` |
| `base64:` | +33% | 多账号且需避免特殊字符 | `echo "base64:$(base64 -w0 /tmp/auth_accounts.json)"` |
| `gzip:` | -60%~80% | **多账号超 64KB 时首选** | `echo "gzip:$(gzip -c /tmp/auth_accounts.json \| base64 -w0)"` |

gzip 实测压缩率：1.5KB → 412B（约 73% 压缩）。多账号场景几乎都能压到 64KB 以内。

---

## 四、HuggingFace Spaces 创建步骤

### 1. 创建 Space

1. 访问 https://huggingface.co/new-space
2. **Owner**: 你的用户名
3. **Space name**: `aistudio2api`
4. **License**: MIT
5. **SDK**: 选择 **Docker**（注意不是 Gradio/Streamlit）
6. **Space hardware**: 
   - **Free CPU (16GB RAM)** —— 足够单账户运行
   - 升级到 2 vCPU 32GB 可获得更稳定体验
7. 可见性：**Private**（强烈推荐，避免 API 被白嫖）

### 2. 上传代码

#### 方式 A：GitHub Actions 自动同步（推荐）

仓库自带两条 CI 工作流，push 到 main 后全自动完成"构建镜像 → 推送 GHCR → 同步代码到 HF Space"：

```
push/手动触发
  └─ docker-publish.yml   构建 Docker 镜像 → 推送 ghcr.io/<owner>/aistudio2api
      └─ huggingface-sync.yml (workflow_call) → 幂等创建/复用 HF Space → 强推源码
```

在 GitHub 仓库 **Settings → Secrets and variables → Actions** 配置：

| 类型 | Name | Value |
|------|------|-------|
| Secret | `HF_TOKEN` | HuggingFace Access Token（需 write 权限，https://huggingface.co/settings/tokens 创建） |
| Variable | `HF_SPACE_OWNER` | 你的 HF 用户名或组织名 |

配置完成后 push 到 main 即自动触发。Space 不存在时 workflow 会自动创建（默认 **private** + Docker SDK）。

> 未配置 `HF_TOKEN` / `HF_SPACE_OWNER` 时同步步骤自动跳过（仅告警不失败），不影响镜像构建。

#### 方式 B：手动推送

将本仓库（含改造后的 Dockerfile、`internal/app/auth_env.go`、`internal/app/app.go` 修改）推送到 Space：

```bash
cd AIStudio2API
git remote add space https://huggingface.co/spaces/<你的用户名>/aistudio2api
git push space main
```

或者用 HF Web 界面直接上传文件。

### 3. 配置 Variables 和 Secrets

在 Space 的 **Settings → Variables and secrets** 中添加：

#### Secrets（敏感数据，加密存储）

| Name | Value | 说明 |
|------|-------|------|
| `AISTUDIO_AUTH_ACCOUNTS` | `gzip:...` 或 `base64:...` 或 JSON 明文 | 多账号凭证数组，gzip 推荐用于多账号 |
| `PROXY_API_KEY` | `sk-your-random-secret` | **必填**，公开 API 鉴权 key，否则任何人都能调用 |
| `ADMIN_TOKEN` | `your-admin-secret` | **HF 必填**，管理端鉴权 token。不设置则 `/api/*` 和管理界面因 loopback 限制全部 403 |

#### Variables（非敏感，明文）

| Name | Value | 说明 |
|------|-------|------|
| `AISTUDIO_ACTIVE_EMAIL` | `account1@gmail.com` | 启动时启用的账号（可选，默认第一个） |
| `AISTUDIO_FAILOVER` | `true` | **HF 推荐**，active 账号失效自动切换到备用账号 |
| `PROXY` | `http://your-proxy:port` | 可选，访问 Google 的代理（HF IP 可能被风控） |
| `TZ` | `Asia/Shanghai` | 时区 |
| `LANG` | `en_US.UTF-8` | 语言环境 |

> **关键**：HF Spaces 反代到容器的 `RemoteAddr` 不是 loopback，原项目默认的 `loopbackMiddleware` 会让所有管理操作（账号切换、启停服务）返回 403。**必须设置 `ADMIN_TOKEN`** 才能解锁管理端。

### 4. 启动

Space 会自动构建 Docker 镜像并启动。可在 **Logs** 标签查看构建日志，应看到：

```
环境变量凭证已就绪 accounts=account1@gmail.com,account2@gmail.com active=account1@gmail.com root=/app/auth
管理端鉴权已启用 | 模式=token
账号失效自动切换已启用 check_interval=30s
管理监听启动 | 地址=0.0.0.0:7860
运行时装配 | 1/3 | 载入账户
运行时装配 | 2/3 | 校验 Camoufox | 账户=2
运行时装配 | 3/3 | 创建协议客户端
协议运行时就绪 | 账户=2 | 耗时=...
```

访问 `https://<你的用户名>-aistudio2api.hf.space/` 浏览器会弹 Basic Auth：
- **用户名**：你设置的 `ADMIN_TOKEN` 值
- **密码**：留空

通过后即可看到管理界面。

---

## 五、账号切换

启动时只有 `AISTUDIO_ACTIVE_EMAIL`（或第一个）账号 `Enabled=true`，其余账号 `Enabled=false`。

### 方式 A：Web 管理界面（手动）

1. 打开管理界面 `https://<你的用户名>-aistudio2api.hf.space/`
   - 浏览器弹 Basic Auth：用户名填 `ADMIN_TOKEN`，密码留空
2. 进入 **账户** 页
3. 找到当前 `Enabled=true` 的账号 → 点击 **停用**
4. 找到目标账号 → 点击 **启用**
5. 回到 **日志** 页确认新账号已就绪

**无需重启 Space**，切换即时生效。

### 方式 B：API 调用（自动化）

```bash
# 停用当前账号
curl -X PUT "https://<user>-aistudio2api.hf.space/api/accounts/account1@gmail.com" \
  -H "X-Admin-Token: $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"label":"account1@gmail.com","enabled":false,"locale":"en-US","timezone":"UTC"}'

# 启用目标账号(字段需完整,从 GET /api/accounts 拷贝)
curl -X PUT "https://<user>-aistudio2api.hf.space/api/accounts/account2@gmail.com" \
  -H "X-Admin-Token: $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"label":"account2@gmail.com","enabled":true,"locale":"en-US","timezone":"UTC"}'
```

### 方式 C：自动 failover（推荐）

设置 `AISTUDIO_FAILOVER=true`（Docker 镜像默认开启）。后台每 30 秒检查一次：
- 若 active 账号变为 `auth_required` 或 `unavailable`
- 自动禁用当前账号，并将其拉黑（本进程内不再选中，防止失效账号被反复启用震荡切换）
- 自动启用列表中下一个未被拉黑的备用账号
- 若备用账号启用后也失效，下一轮检查会继续切换，最终收敛到健康账号
- 全程无需人工干预；进程重启后拉黑状态清零、账号状态从环境变量重新注入

### 重启后行为

HF Space 重启（如免费层空闲超时被回收）会重新读取环境变量，恢复到 `AISTUDIO_ACTIVE_EMAIL` 指定的初始状态。**Web 界面里切换的状态不会持久化**。

如果希望默认启用某个账号，修改 Variables 中的 `AISTUDIO_ACTIVE_EMAIL` 后重启 Space 即可。

---

## 六、调用 API

```bash
# 测试连通性
curl https://<你的用户名>-aistudio2api.hf.space/health

# 列出模型
curl https://<你的用户名>-aistudio2api.hf.space/v1/models \
  -H "Authorization: Bearer $PROXY_API_KEY"

# OpenAI Chat
curl https://<你的用户名>-aistudio2api.hf.space/v1/chat/completions \
  -H "Authorization: Bearer $PROXY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.7-flash",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": false
  }'
```

第三方客户端（Cherry Studio / NextChat / LobeChat 等）：

| 协议 | Base URL | API Key |
|------|----------|---------|
| OpenAI Chat / Responses | `https://<user>-aistudio2api.hf.space/v1` | `PROXY_API_KEY` |
| Anthropic Messages | `https://<user>-aistudio2api.hf.space` | `PROXY_API_KEY` |
| Gemini | `https://<user>-aistudio2api.hf.space` | `PROXY_API_KEY` |

---

## 七、资源占用参考

| 场景 | 内存 | 启动耗时 |
|------|------|----------|
| 1 账号 / Free 16GB | ~1.2GB（Camoufox + Go runtime） | 60-90s（含 Camoufox 预热） |
| 2 账号（1 在线 + 1 备用） | ~1.3GB | 60-90s |
| 5 账号（1 在线 + 4 备用） | ~1.5GB | 60-90s（仅 active 账号初始化 Camoufox） |

> 备用账号 `Enabled=false` 时不启动 Camoufox，几乎不消耗额外内存。

---

## 八、常见问题

### Q1: 启动日志报 `storage state 缺少有效 Cookie: SAPISID`

`storage-state.json` 缺少 Google 认证 Cookie。请重新执行 `setup --login`，确保登录流程完整。如果你是手工导出 cookie，请确保包含 `SAPISID`、`__Secure-1PAPISID`、`__Secure-3PAPISID` 三个 Cookie。

### Q2: 启动报 `AISTUDIO_ACTIVE_EMAIL=xxx 不在注入的账户列表中`

`AISTUDIO_ACTIVE_EMAIL` 的邮箱必须出现在 `AISTUDIO_AUTH_ACCOUNTS` 数组中。检查拼写和大小写。

### Q3: Space 启动后 API 返回 401

`PROXY_API_KEY` 未设置或不匹配。在 HF Secrets 中设置后，请求时 `Authorization: Bearer <key>` 必须完全一致。

### Q3b: 管理界面或 `/api/*` 返回 401 / 403

- **403 forbidden**: 没有设置 `ADMIN_TOKEN`，导致 `loopbackMiddleware` 拒绝非 loopback 请求。**必须设置 `ADMIN_TOKEN`**。
- **401 unauthorized**: 设置了 `ADMIN_TOKEN` 但请求未带凭证。
  - 浏览器访问：弹 Basic Auth 时用户名填 `ADMIN_TOKEN` 值，密码留空
  - API 调用：加 `X-Admin-Token: <token>` 头

### Q4: 账号频繁进入 `auth_required` 状态

HF Spaces 是共享 IP，Google 容易识别为自动化流量。建议：
1. 在 Variables 中配置 `PROXY` 为住宅代理
2. 开启 `AISTUDIO_FAILOVER=true` 自动切换备用账号
3. 或迁到 VPS 部署（参考主 README）

### Q5: 如何查看实时日志

HF Space 的 **Logs** 标签会实时显示应用输出。也可在管理界面 **日志** 页查看结构化日志（需先通过 Basic Auth）。

### Q6: Free Space 空闲被回收后会怎样

HF Free Spaces 在 48 小时无访问后会休眠。下次访问自动唤醒，但 Camoufox 需要重新启动，约 60-90s 延迟。凭证从环境变量重新注入，状态恢复到 `AISTUDIO_ACTIVE_EMAIL` 初始值。

### Q7: 想用本地 Docker 验证

```bash
docker build -t aistudio2api .

# 准备凭证(示例)
python3 -c "
import json
state = json.load(open('auth/your_email@gmail.com/storage-state.json'))
print(json.dumps([{'email':'your_email@gmail.com','storage_state':state}]))
" > /tmp/auth.json

docker run -p 7860:7860 \
  -e AISTUDIO_AUTH_ACCOUNTS="$(cat /tmp/auth.json)" \
  -e PROXY_API_KEY=test123 \
  -e ADMIN_TOKEN=admin456 \
  -e AISTUDIO_FAILOVER=true \
  aistudio2api
```

浏览器访问 `http://localhost:7860`，Basic Auth 用户名填 `admin456`，密码留空。

### Q8: 多账号凭证超过 HF Secrets 64KB 限制

使用 `gzip:` 前缀压缩。实测 10 个账号的 JSON 约 50KB，gzip 后约 8KB：

```bash
echo "gzip:$(gzip -c /tmp/auth.json | base64 -w0)"
```

### Q9: failover 没有触发

- 确认 `AISTUDIO_FAILOVER=true` 已设置（Docker 镜像默认开启）
- 确认 `AISTUDIO_AUTH_ACCOUNTS` 中有多个账号（仅一个账号时无备用可切）
- 查看日志中是否有 `触发自动账号切换` 字样
- failover 每 30 秒检查一次，最多有 30s 延迟
- 若日志出现 `failover 无可用备用账号`，说明所有备用账号均已失效，需更新 `AISTUDIO_AUTH_ACCOUNTS` 后重启 Space

### Q10: 健康检查返回 503

`/health` 在服务未就绪时返回 503 是正常行为，HF 会等待就绪后才路由流量。如果持续 503 超过 2 分钟：
- 查看 Logs 确认 Camoufox 是否启动成功
- 确认 `PROXY` 配置（如有）能访问 Google
- 确认 storage-state.json 中的 Cookie 仍有效

---

## 九、文件清单

本次改造涉及的文件：

| 文件 | 状态 | 说明 |
|------|------|------|
| `internal/app/auth_env.go` | 新增 | 环境变量凭证注入,支持 JSON/base64/gzip 三种编码 |
| `internal/app/failover.go` | 新增 | active 账号失效自动切换监控 |
| `internal/app/app.go` | 修改 | 调用 `setupAuthFromEnv`、`startFailoverMonitor`，`rootHandler` 加 `ADMIN_TOKEN` |
| `internal/api/router.go` | 修改 | `Config` 增加 `AdminToken` 字段，`/api/*` 改用 `adminAuthMiddleware` |
| `internal/api/middleware.go` | 修改 | 新增 `AdminAuthMiddleware`（X-Admin-Token / Basic Auth / ?admin_token=） |
| `internal/api/admin.go` | 修改 | `/health` 智能返回 503（服务未就绪时） |
| `Dockerfile` | 新增 | 多阶段构建 + Camoufox 动态版本 + HF 默认环境变量 |
| `.github/workflows/docker-publish.yml` | 新增 | CI: 构建镜像推送 GHCR，并通过 workflow_call 链式触发 HF 同步 |
| `.github/workflows/huggingface-sync.yml` | 新增 | CI: 幂等创建 HF Space（private + Docker SDK）并强推源码，支持手动/链式触发 |
| `.dockerignore` | 新增 | 排除运行时数据和构建产物 |
| `.env.example` | 修改 | 增加 HF 部署相关变量说明（`ADMIN_TOKEN`、`AISTUDIO_FAILOVER` 等） |
| `README_HF.md` | 新增 | 本文档 |
