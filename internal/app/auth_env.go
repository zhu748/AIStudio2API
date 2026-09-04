package app

import (
        "bytes"
        "compress/gzip"
        "encoding/base64"
        "encoding/json"
        "fmt"
        "log/slog"
        "net/mail"
        "os"
        "path/filepath"
        "sort"
        "strconv"
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

// authAccountVarPrefix 是单账户编号环境变量的前缀，配合正整数编号使用：
//   AISTUDIO_AUTH_ACCOUNT_1、AISTUDIO_AUTH_ACCOUNT_2 … AISTUDIO_AUTH_ACCOUNT_10、
//   AISTUDIO_AUTH_ACCOUNT_11 ……编号无上限，按数值升序加载。
//
// 每个变量的值只放一个账户，支持两种 JSON 形态（均可叠加 base64:/gzip: 前缀）：
//   1. storage-state.json 文件内容直接粘贴（顶层为 cookies/origins/accountName 等字段）
//   2. 包装对象 {"email":"...","storage_state":{...},"proxy":"...","locale":"...","timezone":"..."}
//
// 账户目录名（邮箱标签）的解析优先级：包装对象 email > aistudio2api 扩展邮箱
// > storage_state 的 accountName 根字段 > account-<编号>@env.local 兜底。
//
// 与 AISTUDIO_AUTH_ACCOUNTS（复数，数组格式）互斥，同时设置直接报错。
const authAccountVarPrefix = "AISTUDIO_AUTH_ACCOUNT_"

// setupAuthFromEnv 从环境变量加载多账户凭证并写入 AuthStates 目录
//
// 支持两种注入方式（互斥，同时设置报错）：
//   - AISTUDIO_AUTH_ACCOUNTS：JSON 数组，一次注入全部账户
//   - AISTUDIO_AUTH_ACCOUNT_1 / _2 / … / _10 / _11：每变量一个账户，
//     可直接粘贴 storage-state.json 文件内容
//
// 行为:
//   - 两种方式均未设置时跳过，保持原有目录扫描逻辑
//   - 非空时清空目标目录下的旧账户，重新生成
//   - 只有 ActiveEmail(或编号最小/数组第一个)账户 Enabled=true，其余 Enabled=false
//   - 单账户在线约束由 .env 中 WARM_WORKER_LIMIT=1 / MAX_ACTIVE_WORKERS=1 保证
//
// 编码格式(按前缀自动识别):
//   - 无前缀:        JSON 明文
//   - "base64:":     base64 编码的 JSON
//   - "gzip:":       gzip 压缩 + base64 编码的 JSON(HF Secrets 64KB 限制场景)
func setupAuthFromEnv(authStatesDir string) error {
        raw := strings.TrimSpace(os.Getenv(authAccountsEnvVar))
        numbered, err := collectNumberedAuthVars()
        if err != nil {
                return err
        }
        if raw == "" && len(numbered) == 0 {
                return nil
        }
        if raw != "" && len(numbered) > 0 {
                return fmt.Errorf("%s 与 %s<编号> 不能同时设置，请二选一", authAccountsEnvVar, authAccountVarPrefix)
        }

        var payload []authAccountEntry
        source := authAccountsEnvVar
        if raw != "" {
                payload, err = decodeAuthAccounts(raw)
                if err != nil {
                        return fmt.Errorf("解析 %s: %w", authAccountsEnvVar, err)
                }
        } else {
                source = fmt.Sprintf("%s1..%d", authAccountVarPrefix, numbered[len(numbered)-1].Number)
                payload = make([]authAccountEntry, 0, len(numbered))
                for _, item := range numbered {
                        entry, entryErr := decodeSingleAuthVar(item)
                        if entryErr != nil {
                                return entryErr
                        }
                        payload = append(payload, entry)
                }
        }
        if len(payload) == 0 {
                return fmt.Errorf("注入的账户列表为空")
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

        // 条目缺少 email 时:优先从 storage_state 的 accountName 根字段推导,
        // 编号变量再兜底 account-<编号>@env.local,保证每个凭证都能确定目录名。
        // 必须在默认 active 解析之前完成,否则无 email 条目无法选出默认账户
        for index := range payload {
                fallback := ""
                if len(numbered) > 0 {
                        fallback = fmt.Sprintf("account-%d@env.local", numbered[index].Number)
                }
                fillEntryEmailFallback(&payload[index], fallback)
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
                if contains(installed, email) {
                        return fmt.Errorf("账户 %s 重复注入: 请检查各凭证的 email/accountName 是否重复", email)
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
                "source", source,
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

// decodeAuthAccounts 解析数组格式(兼容旧版)的凭证注入变量,支持 JSON 明文/base64:/gzip:
func decodeAuthAccounts(raw string) ([]authAccountEntry, error) {
        payload, err := decodeAuthPayload(raw)
        if err != nil {
                return nil, err
        }
        payload = strings.TrimSpace(payload)
        if payload == "" {
                return nil, nil
        }
        var entries []authAccountEntry
        if err := json.Unmarshal([]byte(payload), &entries); err != nil {
                return nil, fmt.Errorf("JSON 解析失败: %w", err)
        }
        return entries, nil
}

// decodeAuthPayload 去掉 base64:/gzip: 前缀,返回解码后的 JSON 文本
//
// gzip 生成命令: gzip -c auth.json | base64 -w0
func decodeAuthPayload(raw string) (string, error) {
        switch {
        case strings.HasPrefix(raw, "gzip:"):
                b64Data := strings.TrimPrefix(raw, "gzip:")
                decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64Data))
                if err != nil {
                        return "", fmt.Errorf("gzip base64 解码失败: %w", err)
                }
                gzReader, err := gzip.NewReader(bytes.NewReader(decoded))
                if err != nil {
                        return "", fmt.Errorf("gzip reader 创建失败: %w", err)
                }
                defer gzReader.Close()
                var buf bytes.Buffer
                if _, err := buf.ReadFrom(gzReader); err != nil {
                        return "", fmt.Errorf("gzip 解压失败: %w", err)
                }
                return buf.String(), nil
        case strings.HasPrefix(raw, "base64:"):
                decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "base64:"))
                if err != nil {
                        return "", fmt.Errorf("base64 解码失败: %w", err)
                }
                return string(decoded), nil
        }
        return raw, nil
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

// numberedAuthVar 记录一个编号账户环境变量
type numberedAuthVar struct {
        Number int
        Key    string
        Value  string
}

// collectNumberedAuthVars 扫描进程中所有 AISTUDIO_AUTH_ACCOUNT_<数字> 变量,
// 按编号数值升序返回(字典序会把 _10 排在 _2 之前,必须数值排序)。
//
// 命名约束:
//   - 后缀必须是纯数字正整数,允许前导零但按数值归一
//   - 同一编号只能出现一次(_1 与 _01 视为重复,直接报错)
//   - 变量值不能为空
//   - 非数字后缀(如 AISTUDIO_AUTH_ACCOUNT_ONE)视为命名错误,
//     避免拼错编号导致账户被静默丢弃
func collectNumberedAuthVars() ([]numberedAuthVar, error) {
        found := make([]numberedAuthVar, 0, 8)
        seen := make(map[int]string)
        for _, item := range os.Environ() {
                key, value, _ := strings.Cut(item, "=")
                if !strings.HasPrefix(key, authAccountVarPrefix) {
                        continue
                }
                suffix := strings.TrimPrefix(key, authAccountVarPrefix)
                if suffix == "" || !isAllDigits(suffix) {
                        return nil, fmt.Errorf("环境变量 %s 命名无效: 后缀必须是正整数编号(如 %s1、%s10)",
                                key, authAccountVarPrefix, authAccountVarPrefix)
                }
                number, err := strconv.Atoi(suffix)
                if err != nil || number < 1 {
                        return nil, fmt.Errorf("环境变量 %s 编号超出支持范围", key)
                }
                if previous, exists := seen[number]; exists {
                        return nil, fmt.Errorf("账户编号 %d 重复: %s 与 %s 指向同一编号", number, previous, key)
                }
                seen[number] = key
                trimmed := strings.TrimSpace(value)
                if trimmed == "" {
                        return nil, fmt.Errorf("环境变量 %s 为空: 未提供账户凭证", key)
                }
                found = append(found, numberedAuthVar{Number: number, Key: key, Value: trimmed})
        }
        sort.Slice(found, func(left, right int) bool {
                return found[left].Number < found[right].Number
        })
        return found, nil
}

// isAllDigits 判断字符串是否全为 ASCII 数字且非空
func isAllDigits(value string) bool {
        if value == "" {
                return false
        }
        for _, char := range value {
                if char < '0' || char > '9' {
                        return false
                }
        }
        return true
}

// decodeSingleAuthVar 解析单个编号账户变量的值。
//
// 支持两种 JSON 形态(均可叠加 base64:/gzip: 前缀):
//  1. storage-state.json 文件内容直接粘贴(顶层为 cookies/origins/accountName 等)
//  2. 包装对象 {"email":"...","storage_state":{...},"proxy":"...","locale":"...","timezone":"..."}
//
// 数组形态属于 AISTUDIO_AUTH_ACCOUNTS(复数)专用,在此直接报错并给出提示。
func decodeSingleAuthVar(item numberedAuthVar) (authAccountEntry, error) {
        payload, err := decodeAuthPayload(item.Value)
        if err != nil {
                return authAccountEntry{}, fmt.Errorf("解析 %s: %w", item.Key, err)
        }
        payload = strings.TrimSpace(payload)
        if payload == "" {
                return authAccountEntry{}, fmt.Errorf("%s 解码后内容为空", item.Key)
        }
        if payload[0] == '[' {
                return authAccountEntry{}, fmt.Errorf("%s 的值是数组: 单账户编号变量请直接粘贴 storage-state.json 或使用包装对象,多账户数组请改用 %s",
                        item.Key, authAccountsEnvVar)
        }
        var probe map[string]json.RawMessage
        if err := json.Unmarshal([]byte(payload), &probe); err != nil {
                return authAccountEntry{}, fmt.Errorf("%s JSON 解析失败: %w", item.Key, err)
        }
        var entry authAccountEntry
        if _, wrapped := probe["storage_state"]; wrapped {
                if err := json.Unmarshal([]byte(payload), &entry); err != nil {
                        return authAccountEntry{}, fmt.Errorf("%s JSON 解析失败: %w", item.Key, err)
                }
                return entry, nil
        }
        // 整个对象就是 storage state,原样交给 entry.StorageState,
        // 后续 parseStorageState/Signer 会校验其完整性
        entry.StorageState = json.RawMessage(payload)
        return entry, nil
}

// storageStateAccountEmail 尝试从 storage state 的 accountName 根字段提取邮箱。
// 该字段常见于手工导出的凭证文件,项目运行时不依赖它,
// 仅在条目缺少 email 与 aistudio2api 扩展时用作账户目录名的兜底来源。
func storageStateAccountEmail(raw json.RawMessage) string {
        if len(raw) == 0 {
                return ""
        }
        var probe struct {
                AccountName string `json:"accountName"`
        }
        if err := json.Unmarshal(raw, &probe); err != nil {
                return ""
        }
        candidate := strings.TrimSpace(probe.AccountName)
        if candidate == "" {
                return ""
        }
        address, err := mail.ParseAddress(candidate)
        if err != nil || !strings.EqualFold(strings.TrimSpace(address.Address), candidate) {
                return ""
        }
        return strings.ToLower(address.Address)
}

// fillEntryEmailFallback 为缺少 email 的条目补齐账户标签:
// 显式 email 与 aistudio2api 扩展邮箱优先(见 pickEntryEmail),
// 其次 storage_state 的 accountName 根字段,最后编号兜底(数组格式无编号则不兜底,
// 保持原有"缺少 email"报错行为)
func fillEntryEmailFallback(entry *authAccountEntry, fallback string) {
        if _, err := pickEntryEmail(*entry); err == nil {
                return
        }
        if email := storageStateAccountEmail(entry.StorageState); email != "" {
                entry.Email = email
                return
        }
        if fallback != "" {
                entry.Email = fallback
        }
}
