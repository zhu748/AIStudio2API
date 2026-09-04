package app

import (
        "bytes"
        "compress/gzip"
        "encoding/base64"
        "encoding/json"
        "fmt"
        "os"
        "path/filepath"
        "strings"
        "testing"
)

// testStorageStateJSON 构造可通过 Signer 校验的最小 storage-state JSON。
// accountName 非空时写入根字段,模拟手工导出的凭证文件
// (真实文件顶层形如 {"cookies":[...],"origins":[],"accountName":"xx@gmail.com"})。
func testStorageStateJSON(t *testing.T, accountName string) string {
        t.Helper()
        cookies := []map[string]any{
                {"name": "SAPISID", "value": "test-sapisid", "domain": ".google.com", "path": "/", "expires": 4102444800.0, "httpOnly": false, "secure": true, "sameSite": "None"},
                {"name": "__Secure-1PAPISID", "value": "test-1papisid", "domain": ".google.com", "path": "/", "expires": 4102444800.0, "httpOnly": true, "secure": true, "sameSite": "None"},
                {"name": "__Secure-3PAPISID", "value": "test-3papisid", "domain": ".google.com", "path": "/", "expires": 4102444800.0, "httpOnly": true, "secure": true, "sameSite": "None"},
        }
        state := map[string]any{"cookies": cookies, "origins": []any{}}
        if accountName != "" {
                state["accountName"] = accountName
        }
        data, err := json.Marshal(state)
        if err != nil {
                t.Fatalf("构造 storage state 失败: %v", err)
        }
        return string(data)
}

// gzipTestValue 生成 gzip: 前缀编码的注入值
func gzipTestValue(t *testing.T, payload string) string {
        t.Helper()
        var buf bytes.Buffer
        writer := gzip.NewWriter(&buf)
        if _, err := writer.Write([]byte(payload)); err != nil {
                t.Fatalf("gzip 压缩失败: %v", err)
        }
        if err := writer.Close(); err != nil {
                t.Fatalf("gzip 关闭失败: %v", err)
        }
        return "gzip:" + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// readAccountConfig 读取注入后落盘的 account.json
func readAccountConfig(t *testing.T, root, email string) map[string]any {
        t.Helper()
        data, err := os.ReadFile(filepath.Join(root, email, "account.json"))
        if err != nil {
                t.Fatalf("读取 %s 的 account.json 失败: %v", email, err)
        }
        var config map[string]any
        if err := json.Unmarshal(data, &config); err != nil {
                t.Fatalf("解析 account.json 失败: %v", err)
        }
        return config
}

// requireAccountEnabled 断言账户 enabled 状态
func requireAccountEnabled(t *testing.T, root, email string, enabled bool) {
        t.Helper()
        config := readAccountConfig(t, root, email)
        actual, _ := config["enabled"].(bool)
        if actual != enabled {
                t.Fatalf("账户 %s enabled = %v, 期望 %v", email, actual, enabled)
        }
        if _, err := os.Stat(filepath.Join(root, email, "storage-state.json")); err != nil {
                t.Fatalf("账户 %s 的凭证未落盘: %v", email, err)
        }
}

// 编号变量基本行为: _1/_2/_10 注入,默认启用编号最小的账户
func TestSetupAuthFromEnvNumberedBasic(t *testing.T) {
        root := t.TempDir()
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_1", testStorageStateJSON(t, "one@gmail.com"))
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_2", testStorageStateJSON(t, "two@gmail.com"))
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_10", testStorageStateJSON(t, "ten@gmail.com"))

        if err := setupAuthFromEnv(root); err != nil {
                t.Fatalf("注入失败: %v", err)
        }
        requireAccountEnabled(t, root, "one@gmail.com", true)
        requireAccountEnabled(t, root, "two@gmail.com", false)
        requireAccountEnabled(t, root, "ten@gmail.com", false)
}

// AISTUDIO_ACTIVE_EMAIL 指定编号变量的启用账户
func TestSetupAuthFromEnvNumberedActiveEmail(t *testing.T) {
        root := t.TempDir()
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_1", testStorageStateJSON(t, "one@gmail.com"))
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_2", testStorageStateJSON(t, "two@gmail.com"))
        t.Setenv("AISTUDIO_ACTIVE_EMAIL", "two@gmail.com")

        if err := setupAuthFromEnv(root); err != nil {
                t.Fatalf("注入失败: %v", err)
        }
        requireAccountEnabled(t, root, "one@gmail.com", false)
        requireAccountEnabled(t, root, "two@gmail.com", true)
}

// 编号排序必须是数值序: 只设置 _2 与 _10 时默认启用 _2,
// 字典序会把 _10 排在 _2 之前导致错误的默认账户
func TestSetupAuthFromEnvNumberedNumericOrdering(t *testing.T) {
        root := t.TempDir()
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_10", testStorageStateJSON(t, "ten@gmail.com"))
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_2", testStorageStateJSON(t, "two@gmail.com"))

        if err := setupAuthFromEnv(root); err != nil {
                t.Fatalf("注入失败: %v", err)
        }
        requireAccountEnabled(t, root, "two@gmail.com", true)
        requireAccountEnabled(t, root, "ten@gmail.com", false)
}

// 凭证缺少 email 与扩展时按编号兜底: account-<编号>@env.local
func TestSetupAuthFromEnvNumberedFallbackLabel(t *testing.T) {
        root := t.TempDir()
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_3", testStorageStateJSON(t, ""))

        if err := setupAuthFromEnv(root); err != nil {
                t.Fatalf("注入失败: %v", err)
        }
        requireAccountEnabled(t, root, "account-3@env.local", true)
}

// 包装对象形态: 显式 email 覆盖 accountName,proxy/locale 等字段生效
func TestSetupAuthFromEnvNumberedWrapper(t *testing.T) {
        root := t.TempDir()
        wrapper := map[string]any{
                "email":         "custom+123456@gmail.com",
                "storage_state": json.RawMessage(testStorageStateJSON(t, "ignored@gmail.com")),
                "proxy":         "http://user-residential:8080",
        }
        data, err := json.Marshal(wrapper)
        if err != nil {
                t.Fatalf("构造包装对象失败: %v", err)
        }
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_1", string(data))

        if err := setupAuthFromEnv(root); err != nil {
                t.Fatalf("注入失败: %v", err)
        }
        requireAccountEnabled(t, root, "custom+123456@gmail.com", true)
        config := readAccountConfig(t, root, "custom+123456@gmail.com")
        if proxy, _ := config["proxy"].(string); proxy != "http://user-residential:8080" {
                t.Fatalf("账户级 proxy 未生效: %v", config["proxy"])
        }
        if _, err := os.Stat(filepath.Join(root, "ignored@gmail.com")); !os.IsNotExist(err) {
                t.Fatalf("包装对象 email 未覆盖 accountName: ignored@gmail.com 目录不应存在")
        }
}

// gzip: 前缀编码的编号变量
func TestSetupAuthFromEnvNumberedGzip(t *testing.T) {
        root := t.TempDir()
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_1", gzipTestValue(t, testStorageStateJSON(t, "gz@gmail.com")))

        if err := setupAuthFromEnv(root); err != nil {
                t.Fatalf("注入失败: %v", err)
        }
        requireAccountEnabled(t, root, "gz@gmail.com", true)
}

// 数组值属于 AISTUDIO_AUTH_ACCOUNTS 专用,编号变量直接报错
func TestSetupAuthFromEnvNumberedRejectsArray(t *testing.T) {
        root := t.TempDir()
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_1", `[{"email":"a@gmail.com"}]`)

        err := setupAuthFromEnv(root)
        if err == nil || !strings.Contains(err.Error(), authAccountsEnvVar) {
                t.Fatalf("期望数组值报错并提示改用 %s, 实际: %v", authAccountsEnvVar, err)
        }
}

// 复数数组变量与编号变量互斥
func TestSetupAuthFromEnvRejectsBothSources(t *testing.T) {
        root := t.TempDir()
        t.Setenv("AISTUDIO_AUTH_ACCOUNTS", "[]")
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_1", testStorageStateJSON(t, "one@gmail.com"))

        err := setupAuthFromEnv(root)
        if err == nil || !strings.Contains(err.Error(), "不能同时设置") {
                t.Fatalf("期望互斥报错, 实际: %v", err)
        }
}

// 非数字后缀视为命名错误
func TestSetupAuthFromEnvRejectsInvalidSuffix(t *testing.T) {
        root := t.TempDir()
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_ONE", testStorageStateJSON(t, "a@gmail.com"))

        err := setupAuthFromEnv(root)
        if err == nil || !strings.Contains(err.Error(), "命名无效") {
                t.Fatalf("期望命名无效报错, 实际: %v", err)
        }
}

// _1 与 _01 指向同一编号,视为重复
func TestSetupAuthFromEnvRejectsDuplicateNumber(t *testing.T) {
        root := t.TempDir()
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_1", testStorageStateJSON(t, "a@gmail.com"))
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_01", testStorageStateJSON(t, "b@gmail.com"))

        err := setupAuthFromEnv(root)
        if err == nil || !strings.Contains(err.Error(), "重复") {
                t.Fatalf("期望编号重复报错, 实际: %v", err)
        }
}

// 两个凭证解析出同一邮箱时拒绝注入,防止后写的静默覆盖先写的
func TestSetupAuthFromEnvRejectsDuplicateEmail(t *testing.T) {
        root := t.TempDir()
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_1", testStorageStateJSON(t, "dup@gmail.com"))
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_2", testStorageStateJSON(t, "dup@gmail.com"))

        err := setupAuthFromEnv(root)
        if err == nil || !strings.Contains(err.Error(), "重复注入") {
                t.Fatalf("期望邮箱重复报错, 实际: %v", err)
        }
}

// 编号变量值为空白时报错
func TestSetupAuthFromEnvRejectsEmptyValue(t *testing.T) {
        root := t.TempDir()
        t.Setenv("AISTUDIO_AUTH_ACCOUNT_1", "   ")

        err := setupAuthFromEnv(root)
        if err == nil || !strings.Contains(err.Error(), "为空") {
                t.Fatalf("期望空值报错, 实际: %v", err)
        }
}

// 旧版数组格式回归: 行为不变,默认启用第一个
func TestSetupAuthFromEnvArrayRegression(t *testing.T) {
        root := t.TempDir()
        entries := fmt.Sprintf(`[{"email":"arr1@gmail.com","storage_state":%s},{"email":"arr2@gmail.com","storage_state":%s}]`,
                testStorageStateJSON(t, ""), testStorageStateJSON(t, ""))
        t.Setenv("AISTUDIO_AUTH_ACCOUNTS", entries)

        if err := setupAuthFromEnv(root); err != nil {
                t.Fatalf("注入失败: %v", err)
        }
        requireAccountEnabled(t, root, "arr1@gmail.com", true)
        requireAccountEnabled(t, root, "arr2@gmail.com", false)
}

// 数组格式同样受益于 accountName 兜底
func TestSetupAuthFromEnvArrayAccountNameFallback(t *testing.T) {
        root := t.TempDir()
        entries := fmt.Sprintf(`[{"storage_state":%s}]`, testStorageStateJSON(t, "named@gmail.com"))
        t.Setenv("AISTUDIO_AUTH_ACCOUNTS", entries)

        if err := setupAuthFromEnv(root); err != nil {
                t.Fatalf("注入失败: %v", err)
        }
        requireAccountEnabled(t, root, "named@gmail.com", true)
}

// 未设置任何注入变量时保持目录扫描逻辑,不报错
func TestSetupAuthFromEnvSkipsWhenUnset(t *testing.T) {
        root := t.TempDir()
        if err := setupAuthFromEnv(root); err != nil {
                t.Fatalf("无注入变量时不应报错: %v", err)
        }
}

// storageStateAccountEmail 从 accountName 根字段提取邮箱
func TestStorageStateAccountEmail(t *testing.T) {
        cases := []struct {
                name  string
                raw   string
                want  string
        }{
                {"邮箱", `{"accountName":"xx@gmail.com"}`, "xx@gmail.com"},
                {"大写归一", `{"accountName":"Mixed@GMAIL.com"}`, "mixed@gmail.com"},
                {"非邮箱", `{"accountName":"John Doe"}`, ""},
                {"缺失", `{"cookies":[]}`, ""},
                {"空值", `{"accountName":" "}`, ""},
        }
        for _, item := range cases {
                t.Run(item.name, func(t *testing.T) {
                        if got := storageStateAccountEmail(json.RawMessage(item.raw)); got != item.want {
                                t.Fatalf("storageStateAccountEmail = %q, 期望 %q", got, item.want)
                        }
                })
        }
}
