package app

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// TestRequestRegistryLifecycleUnderLogFlood 验证拆锁后请求生命周期操作
// 不被高频日志扇出阻塞:并发 start/markRunning/finish 与日志风暴同时进行,
// 在有限时间内完成且终态一致(全部 finish 后 count 为 0)。
func TestRequestRegistryLifecycleUnderLogFlood(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := newRequestRegistry(ctx)

	const requestCount = 64
	const logCount = 2000
	var wg sync.WaitGroup
	done := make(chan struct{})

	// 日志风暴 + 订阅者扇出压力
	subscriberCtx, subscriberCancel := context.WithCancel(ctx)
	events := registry.activateSubscriber(
		registry.subscribe(subscriberCtx), nil, nil,
	)
	go func() {
		for range events {
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for index := range logCount {
			registry.log("test", "INFO", fmt.Sprintf("日志条目 %d", index))
		}
	}()

	// 请求生命周期风暴
	wg.Add(1)
	go func() {
		defer wg.Done()
		var lifecycle sync.WaitGroup
		for index := range requestCount {
			lifecycle.Add(1)
			go func(index int) {
				defer lifecycle.Done()
				request := aistudio.GenerateRequest{ID: fmt.Sprintf("req-%d", index), Model: "gemini-flash-latest"}
				registry.start(request, func() {})
				registry.markRunning(request.ID, "account-1", "Account 1")
				registry.finish(request.ID, "completed", nil)
			}(index)
		}
		lifecycle.Wait()
	}()

	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("请求生命周期被日志扇出阻塞(可能发生死锁或锁饥饿)")
	}

	deadline := time.Now().Add(2 * time.Second)
	for registry.count() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if count := registry.count(); count != 0 {
		t.Fatalf("全部请求结束后活动计数应为 0,实际 %d", count)
	}
	subscriberCancel()
	registry.clearLogs()
	if requests := registry.list(); len(requests) != 0 {
		t.Fatalf("全部请求结束后列表应为空,实际 %d", len(requests))
	}
}

// TestRequestRegistrySubscriberSeesActiveRequest 验证订阅激活时不丢失
// 已在运行中的请求(拆锁后 activateSubscriber 的快照一致性)。
func TestRequestRegistrySubscriberSeesActiveRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := newRequestRegistry(ctx)

	running := aistudio.GenerateRequest{ID: "req-running", Model: "gemini-flash-latest"}
	registry.start(running, func() {})
	registry.markRunning(running.ID, "account-1", "Account 1")

	eventCtx, eventCancel := context.WithCancel(ctx)
	stream := registry.activateSubscriber(registry.subscribe(eventCtx), nil, nil)
	found := false
	readDeadline := time.After(2 * time.Second)
	for !found {
		select {
		case event, ok := <-stream:
			if !ok {
				t.Fatal("事件流提前关闭,未见运行中的请求")
			}
			if event.Type == "request" {
				if request, casted := event.Data.(api.AdminRequest); casted && request.ID == running.ID {
					found = true
				}
			}
		case <-readDeadline:
			t.Fatal("订阅激活快照未包含运行中的请求")
		}
	}
	registry.finish(running.ID, "completed", nil)
	eventCancel()
}

// TestWorkerScansDoNotBlockOnInflightRPC 验证 H1 修复:当某账户的
// account.mu 被模拟的在途浏览器 RPC 长期持有时,全局扫描路径
// (ReadyWarmAccountIDs/occupiedWorkers/idleWarmVictim)必须在
// 远短于持锁时间的窗口内完成(TryLock 跳过而非阻塞)。
// 修复前这些扫描阻塞在 account.mu 上并(经 rebalanceMu)冻结全部调度。
func TestWorkerScansDoNotBlockOnInflightRPC(t *testing.T) {
	pool := aistudio.NewAccountPool(nil, 2)
	accounts := make([]*aistudio.Account, 0, 4)
	for index := range 4 {
		accounts = append(accounts, &aistudio.Account{
			ID:     fmt.Sprintf("account-%d", index),
			Config: aistudio.AccountConfig{Label: fmt.Sprintf("账户 %d", index), Enabled: true},
		})
	}
	manager := newAccountWorkerManager(pool, accounts, newRequestRegistry(t.Context()), "/nonexistent-camoufox", "", time.Second, 2, 4, 2, false)
	defer manager.cancel()

	// 账户 1 模拟在途 RPC:持 account.mu 300ms
	blocked := manager.accounts["account-1"]
	blocked.mu.Lock()
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		for range 5 {
			manager.ReadyWarmAccountIDs()
			manager.occupiedWorkers()
			manager.idleWarmVictim("")
		}
	}()
	select {
	case <-scanDone:
		// 5 轮扫描在持锁期间完成 → 未被阻塞
	case <-time.After(250 * time.Millisecond):
		blocked.mu.Unlock()
		t.Fatal("扫描路径被在途 RPC 的 account.mu 阻塞(H1 回归)")
	}
	blocked.mu.Unlock()
	select {
	case <-scanDone:
	case <-time.After(2 * time.Second):
		t.Fatal("扫描 goroutine 未退出")
	}
}
