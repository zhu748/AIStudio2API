package aistudio

import (
	"testing"
	"time"
)

// TestModelsCacheLifecycle 覆盖 modelsCache 的存取/过期/失效语义:
// /v1/models TTL 缓存(30s)与 RefreshAccountModels 主动失效依赖这些行为。
func TestModelsCacheLifecycle(t *testing.T) {
	var cache modelsCache

	if _, ok := cache.snapshot(); ok {
		t.Fatal("空缓存不应命中")
	}

	cache.store([]Model{{ID: "gemini-flash-latest"}}, time.Minute)
	models, ok := cache.snapshot()
	if !ok {
		t.Fatal("写入后应命中缓存")
	}
	if len(models) != 1 || models[0].ID != "gemini-flash-latest" {
		t.Fatalf("缓存内容不符: %+v", models)
	}

	// 过期后不命中
	cache.store([]Model{{ID: "m"}}, -time.Second)
	if _, ok := cache.snapshot(); ok {
		t.Fatal("过期缓存不应命中")
	}

	// 主动失效后不命中
	cache.store([]Model{{ID: "m"}}, time.Hour)
	cache.invalidate()
	if _, ok := cache.snapshot(); ok {
		t.Fatal("invalidate 后不应命中")
	}

	// nil 结果不缓存:失败的刷新不应让后续调用拿到空目录
	cache.store(nil, time.Hour)
	if _, ok := cache.snapshot(); ok {
		t.Fatal("nil 模型列表不应命中")
	}
}
