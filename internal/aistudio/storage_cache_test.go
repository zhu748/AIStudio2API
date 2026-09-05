package aistudio

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStorageStateReadCacheHit 验证 stat 短路:磁盘内容被同尺寸篡改且
// mtime 保持不变时,读缓存返回上次解析结果而非磁盘新内容。
func TestStorageStateReadCacheHit(t *testing.T) {
	account, pool, lease := leaseWithStorageFile(t, "cache-hit", "v1")
	defer lease.Release()

	state, err := lease.ReloadStorageState()
	if err != nil {
		t.Fatalf("首次读取失败: %v", err)
	}
	if len(state.Cookies) != 1 || state.Cookies[0].Value != "v1" {
		t.Fatalf("首次读取内容异常: %+v", state.Cookies)
	}

	// 同尺寸篡改磁盘内容 + 保持 mtime/size 不变 → 缓存应命中旧值
	tamperStorageFileSameStat(t, account.StoragePath, "v1", "vX")

	cached, err := lease.ReloadStorageState()
	if err != nil {
		t.Fatalf("缓存读取失败: %v", err)
	}
	if len(cached.Cookies) != 1 || cached.Cookies[0].Value != "v1" {
		t.Fatalf("stat 未变化时应返回缓存值 v1,实际 %+v", cached.Cookies)
	}
	_ = pool
}

// TestStorageStateReadCacheInvalidation 验证 mtime 变化后缓存自动失效重读。
func TestStorageStateReadCacheInvalidation(t *testing.T) {
	account, _, lease := leaseWithStorageFile(t, "cache-invalidate", "v1")
	defer lease.Release()

	if _, err := lease.ReloadStorageState(); err != nil {
		t.Fatalf("首次读取失败: %v", err)
	}

	// 模拟外部进程写入:直接改写文件并推进 mtime
	tamperStorageFileSameStat(t, account.StoragePath, "v1", "v2")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(account.StoragePath, future, future); err != nil {
		t.Fatalf("推进 mtime 失败: %v", err)
	}

	refreshed, err := lease.ReloadStorageState()
	if err != nil {
		t.Fatalf("失效后读取失败: %v", err)
	}
	if len(refreshed.Cookies) != 1 || refreshed.Cookies[0].Value != "v2" {
		t.Fatalf("mtime 变化后应重读磁盘得 v2,实际 %+v", refreshed.Cookies)
	}
}

// TestStorageStateCacheAfterMerge 验证写入路径(MergeSetCookieHeaders)
// 落盘后刷新缓存:后续读取无需重读磁盘即得到合并结果。
func TestStorageStateCacheAfterMerge(t *testing.T) {
	account, _, lease := leaseWithStorageFile(t, "cache-merge", "v1")
	defer lease.Release()

	if _, err := lease.ReloadStorageState(); err != nil {
		t.Fatalf("首次读取失败: %v", err)
	}
	// 合并新 Cookie:触发 读缓存命中 → 合并 → 落盘 → 刷新缓存
	err := lease.MergeSetCookieHeaders(
		[]string{"newcookie=merged; Path=/; Domain=google.com"},
		"https://google.com/path", time.Now(),
	)
	if err != nil {
		t.Fatalf("合并 Cookie 失败: %v", err)
	}

	// 落盘文件中应有 2 条 Cookie;缓存应直接反映
	onDisk, err := LoadStorageState(account.StoragePath)
	if err != nil {
		t.Fatalf("读取磁盘失败: %v", err)
	}
	if len(onDisk.Cookies) != 2 {
		t.Fatalf("磁盘应有 2 条 Cookie,实际 %d", len(onDisk.Cookies))
	}

	// 同 stat 篡改磁盘 → 读取仍返回合并后的缓存值(证明缓存与落盘一致)
	tamperFirstCookieValue(t, account.StoragePath, "merged")
	cached, err := lease.ReloadStorageState()
	if err != nil {
		t.Fatalf("合并后读取失败: %v", err)
	}
	if len(cached.Cookies) != 2 {
		t.Fatalf("合并后缓存应含 2 条 Cookie,实际 %d", len(cached.Cookies))
	}
	if cached.Cookies[1].Value != "merged" {
		t.Fatalf("合并后缓存值异常: %+v", cached.Cookies)
	}
}

// TestStorageStateCloneIsolation 验证 Reload 返回的副本可安全原地修改,
// 不污染缓存底层数组(MergeSetCookieHeaders 会原地增删)。
func TestStorageStateCloneIsolation(t *testing.T) {
	_, _, lease := leaseWithStorageFile(t, "cache-clone", "v1")
	defer lease.Release()

	state, err := lease.ReloadStorageState()
	if err != nil {
		t.Fatalf("首次读取失败: %v", err)
	}
	// 原地删除唯一 Cookie,模拟调用方破坏性修改
	state.Cookies = state.Cookies[:0]

	again, err := lease.ReloadStorageState()
	if err != nil {
		t.Fatalf("二次读取失败: %v", err)
	}
	if len(again.Cookies) != 1 {
		t.Fatalf("调用方原地修改不应影响缓存,实际 %d 条", len(again.Cookies))
	}
}

// leaseWithStorageFile 构造带真实磁盘 storage 文件的账户租约。
// cookieValue 为唯一 Cookie 的值。
func leaseWithStorageFile(t *testing.T, id string, cookieValue string) (*Account, *AccountPool, *AccountLease) {
	t.Helper()
	directory := t.TempDir()
	state := StorageState{Cookies: []StateCookie{{
		Name: "sid", Value: cookieValue,
		Domain: "google.com", Path: "/", Expires: -1,
	}}}
	storagePath := filepath.Join(directory, "storage-state.json")
	if err := WriteStorageState(storagePath, state); err != nil {
		t.Fatalf("写入初始 storage state: %v", err)
	}
	account := testAccount(id, true, AccountReady, testChatModel("gemini-flash-latest"))
	account.StoragePath = storagePath
	pool := NewAccountPool([]*Account{account}, 1)
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{})
	if err != nil {
		t.Fatalf("获取租约失败: %v", err)
	}
	return account, pool, lease
}

// tamperStorageFileSameStat 同尺寸篡改文件内容后恢复 mtime。
func tamperStorageFileSameStat(t *testing.T, path string, oldValue string, newValue string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	replaced := replaceFirst(data, oldValue, newValue)
	if replaced == nil {
		t.Fatalf("未找到待替换值 %q", oldValue)
	}
	if err := os.WriteFile(path, replaced, 0o600); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("恢复 mtime 失败: %v", err)
	}
}

// tamperFirstCookieValue 将文件中第一条 Cookie 的值替换为同长度新值
// 并保持 mtime 不变。
func tamperFirstCookieValue(t *testing.T, path string, newValue string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	var raw map[string]json.RawMessage
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	var cookies []map[string]any
	if err := json.Unmarshal(raw["cookies"], &cookies); err != nil {
		t.Fatalf("解析 cookies 失败: %v", err)
	}
	if len(cookies) == 0 {
		t.Fatal("cookies 为空")
	}
	cookies[0]["value"] = newValue
	encoded, err := json.Marshal(cookies)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	raw["cookies"] = encoded
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("编码整体失败: %v", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("恢复 mtime 失败: %v", err)
	}
}

// replaceFirst 返回替换首个 oldValue 为同长度 newValue 后的字节切片。
func replaceFirst(data []byte, oldValue string, newValue string) []byte {
	if len(oldValue) != len(newValue) {
		return nil
	}
	index := indexOf(string(data), oldValue)
	if index < 0 {
		return nil
	}
	out := append([]byte(nil), data...)
	copy(out[index:], newValue)
	return out
}

func indexOf(haystack string, needle string) int {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return index
		}
	}
	return -1
}
