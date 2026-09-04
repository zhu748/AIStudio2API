package app

import (
        "bytes"
        "compress/gzip"
        "encoding/base64"
        "encoding/json"
        "fmt"
        "log/slog"
        "os"
        "path/filepath"
        "strings"

        "github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// authAccountEntry 描述从环境变量注入的单个账户
//
// Proxy/Locale/Timezone 可选,未指定时使用环境变量 AISTUDIO_DEFAULT_PROXY/
// AISTUDIO_DEFAULT_LOCALE/AISTUDIO_DEFAULT_TIMEZONE,再退回系统默认
type authAccountEntry struct {
        Email        string          `json:"email"`
        StorageState json.RawMessage `json:"storage_state"`
        Proxy        string          `json:"proxy,omitempty"`
        Locale       string          `json:"locale,omitempty"`
        Timezone     string          `json:"timezone,omitempty"`
}

// authAccountsEnvVar 是注入多账户凭证的环境变量名
const authAccountsEnvVar = "AISTUDIO_AUTH_ACCOUNTS"

// activeEmailEnvVar 指定启动时启用的账户邮箱；为空时启用列表中第一个
const activeEmailEnvVar = "AISTUDIO_ACTIVE_EMAIL"

// setupAuthFromEnv 从环境变量加载多账户凭证并写入 AuthStates 目录
//
// 行为:
//   - 环境变量为空时跳过,保持原有目录扫描逻辑
//   - 非空时清空目标目录下的旧账户,按 JSON 数组重新生成
//   - 只有 ActiveEmail(或第一个)账户 Enabled=true,其余 Enabled=false
//   - 单账户在线约束由 .env 中 WARM_WORKER_LIMIT=1 / MAX_ACTIVE_WORKERS=1 保证
//
// 编码格式(按前缀自动识别):
//   - 无前缀:        JSON 明文
//   - "base64:":     base64 编码的 JSON
//   - "gzip:":       gzip 压缩 + base64 编码的 JSON(HF Secrets 64KB 限制场景)
func setupAuthFromEnv(authStatesDir string) error {
        raw := strings.TrimSpace(os.Getenv(authAccountsEnvVar))
        if raw == "" {
                return nil
        }
        payload, err := decodeAuthAccounts(raw)
        if err != nil {
                return fmt.Errorf("解析 %s: %w", authAccountsEnvVar, err)
        }
        if len(payload) == 0 {
                return fmt.Errorf("%s 不能为空数组", authAccountsEnvVar)
        }

        root, err := filepath.Abs(authStatesDir)
        if err != nil {
                return fmt.Errorf("解析 AuthStates 路径: %w", err)
        }
        if err := os.MkdirAll(root, 0o755); err != nil {
                return fmt.Errorf("创建账户根目录: %w", err)
        }
        if err := resetAuthRoot(root); err != nil {
                return fmt.Errorf("清理旧账户目录: %w", err)
        }

        activeEmail := strings.ToLower(strings.TrimSpace(os.Getenv(activeEmailEnvVar)))
        if activeEmail == "" {
                // 默认启用列表第一个账户
                if first, err := pickEntryEmail(payload[0]); err == nil {
                        activeEmail = first
                }
        }

        locale := aistudio.DefaultAccountLocale()
        timezone := aistudio.DefaultAccountTimezone()
        // 单账户级别默认值,可通过环境变量覆盖全部注入账户
        defaultProxy := strings.TrimSpace(os.Getenv("AISTUDIO_DEFAULT_PROXY"))
        if v := strings.TrimSpace(os.Getenv("AISTUDIO_DEFAULT_LOCALE")); v != "" {
                locale = v
        }
        if v := strings.TrimSpace(os.Getenv("AISTUDIO_DEFAULT_TIMEZONE")); v != "" {
                timezone = v
        }

        installed := make([]string, 0, len(payload))
        for _, entry := range payload {
                email, err := pickEntryEmail(entry)
                if err != nil {
                        return fmt.Errorf("解析账户邮箱: %w", err)
                }
                state, err := parseStorageState(entry.StorageState)
                if err != nil {
                        return fmt.Errorf("账户 %s storage_state 无效: %w", email, err)
                }
                if _, err := aistudio.NewSigner().Sign(state); err != nil {
                        return fmt.Errorf("账户 %s 认证状态无法用于 AI Studio: %w", email, err)
                }

                accountDir := filepath.Join(root, email)
                if err := os.MkdirAll(accountDir, 0o700); err != nil {
                        return fmt.Errorf("创建账户目录 %s: %w", email, err)
                }
                config := aistudio.DefaultAccountConfig(email)
                // 账户级别覆盖 > 环境变量默认值 > 系统默认
                if v := strings.TrimSpace(entry.Proxy); v != "" {
                        config.Proxy = v
                } else if defaultProxy != "" {
                        config.Proxy = defaultProxy
                }
                if v := strings.TrimSpace(entry.Locale); v != "" {
                        config.Locale = v
                } else {
                        config.Locale = locale
                }
                if v := strings.TrimSpace(entry.Timezone); v != "" {
                        config.Timezone = v
                } else {
                        config.Timezone = timezone
                }
                config.Enabled = strings.EqualFold(email, activeEmail)
                if err := writeAuthAccountConfig(accountDir, config); err != nil {
                        return fmt.Errorf("写入账户 %s 配置: %w", email, err)
                }
                if err := aistudio.WriteStorageState(filepath.Join(accountDir, "storage-state.json"), state); err != nil {
                        return fmt.Errorf("写入账户 %s 凭证: %w", email, err)
                }
                installed = append(installed, email)
        }

        if activeEmail == "" {
                return fmt.Errorf("%s 需要至少一个有效邮箱", authAccountsEnvVar)
        }
        if !contains(installed, activeEmail) {
                return fmt.Errorf("%s=%s 不在注入的账户列表 %v 中",
                        activeEmailEnvVar, activeEmail, installed)
        }

        slog.Info("环境变量凭证已就绪",
                "accounts", strings.Join(installed, ","),
                "active", activeEmail,
                "root", root,
        )
        return nil
}

// failoverEnabled 返回是否启用 active 账号失效自动切换
// 通过环境变量 AISTUDIO_FAILOVER=true 开启
func failoverEnabled() bool {
        value := strings.ToLower(strings.TrimSpace(os.Getenv("AISTUDIO_FAILOVER")))
        return value == "true" || value == "1" || value == "yes"
}

// decodeAuthAccounts 解析环境变量值,支持 JSON 明文 / base64: / gzip: 三种格式
//
// gzip: 适用于 HF Secrets 64KB 限制场景
// 生成命令: gzip -c auth.json | base64 -w0
func decodeAuthAccounts(raw string) ([]authAccountEntry, error) {
        switch {
        case strings.HasPrefix(raw, "gzip:"):
                b64Data := strings.TrimPrefix(raw, "gzip:")
                decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64Data))
                if err != nil {
                        return nil, fmt.Errorf("gzip base64 解码失败: %w", err)
                }
                gzReader, err := gzip.NewReader(bytes.NewReader(decoded))
                if err != nil {
                        return nil, fmt.Errorf("gzip reader 创建失败: %w", err)
                }
                defer gzReader.Close()
                var buf bytes.Buffer
                if _, err := buf.ReadFrom(gzReader); err != nil {
                        return nil, fmt.Errorf("gzip 解压失败: %w", err)
                }
                raw = buf.String()
        case strings.HasPrefix(raw, "base64:"):
                decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "base64:"))
                if err != nil {
                        return nil, fmt.Errorf("base64 解码失败: %w", err)
                }
                raw = string(decoded)
        }
        raw = strings.TrimSpace(raw)
        if raw == "" {
                return nil, nil
        }
        var entries []authAccountEntry
        if err := json.Unmarshal([]byte(raw), &entries); err != nil {
                return nil, fmt.Errorf("JSON 解析失败: %w", err)
        }
        return entries, nil
}

// pickEntryEmail 从条目中提取小写邮箱
//
// 优先级: 显式 email > storage_state.aistudio2api.source.email
func pickEntryEmail(entry authAccountEntry) (string, error) {
        if email := strings.ToLower(strings.TrimSpace(entry.Email)); email != "" {
                return email, nil
        }
        state, err := parseStorageState(entry.StorageState)
        if err != nil {
                return "", fmt.Errorf("storage_state 无效: %w", err)
        }
        if extension, exists, err := state.AuthExtension(); err != nil {
                return "", err
        } else if exists && strings.TrimSpace(extension.Source.Email) != "" {
                return strings.ToLower(strings.TrimSpace(extension.Source.Email)), nil
        }
        return "", fmt.Errorf("缺少 email 字段且 storage_state 未携带 aistudio2api 扩展")
}

// parseStorageState 解析原始 JSON 为 StorageState
func parseStorageState(raw json.RawMessage) (aistudio.StorageState, error) {
        if len(raw) == 0 {
                return aistudio.StorageState{}, fmt.Errorf("storage_state 为空")
        }
        var state aistudio.StorageState
        if err := json.Unmarshal(raw, &state); err != nil {
                return aistudio.StorageState{}, err
        }
        if err := state.Validate(); err != nil {
                return aistudio.StorageState{}, err
        }
        return state, nil
}

// writeAuthAccountConfig 直接原子写入 account.json,绕过 setup CLI 流程
func writeAuthAccountConfig(dir string, config aistudio.AccountConfig) error {
        if err := config.Validate(); err != nil {
                return err
        }
        data, err := json.MarshalIndent(config, "", "  ")
        if err != nil {
                return fmt.Errorf("编码 account.json: %w", err)
        }
        data = append(data, '\n')
        return os.WriteFile(filepath.Join(dir, "account.json"), data, 0o600)
}

// resetAuthRoot 删除 root 下所有子目录,保留 .leases 等隐藏文件
func resetAuthRoot(root string) error {
        entries, err := os.ReadDir(root)
        if err != nil {
                if os.IsNotExist(err) {
                        return nil
                }
                return err
        }
        for _, entry := range entries {
                if !entry.IsDir() {
                        continue
                }
                name := entry.Name()
                // 保留 .leases 跨进程锁目录
                if strings.HasPrefix(name, ".") {
                        continue
                }
                if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
                        return fmt.Errorf("删除 %s: %w", name, err)
                }
        }
        return nil
}

func contains(values []string, target string) bool {
        for _, value := range values {
                if strings.EqualFold(value, target) {
                        return true
                }
        }
        return false
}
