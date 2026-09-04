package aistudio

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// testAccount 构造带最小目录的测试账户
func testAccount(id string, enabled bool, state AccountState, models ...Model) *Account {
	return &Account{
		ID:          id,
		Config:      AccountConfig{Label: id, Enabled: enabled},
		State:       state,
		Models:      models,
		BenefitTier: BenefitTierFree,
	}
}

// testChatModel 构造满足 WAA bootstrap 资格的模型
func testChatModel(id string) Model {
	return Model{
		ID:           id,
		Name:         id,
		Methods:      []string{"generateContent"},
		Capabilities: map[string]bool{"chat_model": true},
	}
}

// overviewsByID 将轻量视图转为 ID 索引
func overviewsByID(overviews []AccountOverview) map[string]AccountOverview {
	result := make(map[string]AccountOverview, len(overviews))
	for _, overview := range overviews {
		result[overview.ID] = overview
	}
	return result
}

// statusesByID 将脱敏状态转为 ID 索引
func statusesByID(statuses []AccountStatus) map[string]AccountStatus {
	result := make(map[string]AccountStatus, len(statuses))
	for _, status := range statuses {
		result[status.ID] = status
	}
	return result
}

// TestAccountOverviewsMatchesStatusStates 验证轻量视图与全量 Status
// 的状态推导完全一致(含 disabled/busy/cooldown 降级)。
func TestAccountOverviewsMatchesStatusStates(t *testing.T) {
	ready := testAccount("ready", true, AccountReady, testChatModel("gemini-flash-latest"))
	disabled := testAccount("disabled", false, AccountReady)
	authRequired := testAccount("auth", true, AccountAuthRequired)
	cooldown := testAccount("cooldown", true, AccountReady)
	cooldown.runtime.Cooldowns = map[string]CooldownState{
		globalCooldownKey: {Until: time.Now().Add(time.Hour), Reason: "test"},
	}
	unavailable := testAccount("unavailable", true, AccountUnavailable)
	pool := NewAccountPool([]*Account{ready, disabled, authRequired, cooldown, unavailable}, 2)

	statuses := statusesByID(pool.Status())
	overviews := overviewsByID(pool.AccountOverviews())
	if len(statuses) != len(overviews) {
		t.Fatalf("视图数量不一致: status=%d overview=%d", len(statuses), len(overviews))
	}
	for id, status := range statuses {
		overview, exists := overviews[id]
		if !exists {
			t.Fatalf("轻量视图缺少账户 %s", id)
		}
		if overview.State != status.State {
			t.Errorf("账户 %s 状态不一致: status=%s overview=%s", id, status.State, overview.State)
		}
		if overview.Enabled != status.Enabled {
			t.Errorf("账户 %s 启用标记不一致: status=%v overview=%v", id, status.Enabled, overview.Enabled)
		}
		if overview.ID != status.ID || overview.Label != status.Label {
			t.Errorf("账户 %s 标识不一致: overview=%+v", id, overview)
		}
	}
}

// TestAccountOverviewsReflectsBusy 验证租约占用反映为 Busy 状态。
func TestAccountOverviewsReflectsBusy(t *testing.T) {
	account := testAccount("a1", true, AccountReady, testChatModel("gemini-flash-latest"))
	pool := NewAccountPool([]*Account{account}, 2)

	lease, err := pool.AcquireFor(context.Background(), AccountSelection{})
	if err != nil {
		t.Fatalf("获取租约失败: %v", err)
	}
	overview := overviewsByID(pool.AccountOverviews())["a1"]
	if overview.State != AccountBusy {
		t.Errorf("占用后应为 busy,实际 %s", overview.State)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("释放租约失败: %v", err)
	}
	overview = overviewsByID(pool.AccountOverviews())["a1"]
	if overview.State != AccountReady {
		t.Errorf("释放后应为 ready,实际 %s", overview.State)
	}
}

// TestBootstrapModelIDsUnion 验证单次锁聚合与逐账户收集结果一致。
func TestBootstrapModelIDsUnion(t *testing.T) {
	a1 := testAccount("a1", true, AccountReady, testChatModel("gemini-flash-latest"), testChatModel("model-b"))
	a2 := testAccount("a2", true, AccountReady, testChatModel("model-b"), testChatModel("model-c"))
	// 禁用与不可用账户不应贡献
	a3 := testAccount("a3", false, AccountReady, testChatModel("model-d"))
	a4 := testAccount("a4", true, AccountAuthRequired, testChatModel("model-e"))
	pool := NewAccountPool([]*Account{a1, a2, a3, a4}, 2)

	aggregated := pool.BootstrapModelIDs()
	expectedSet := map[string]struct{}{}
	for _, account := range []*Account{a1, a2} {
		for _, modelID := range accountBootstrapModels(account) {
			expectedSet[modelID] = struct{}{}
		}
	}
	if len(aggregated) != len(expectedSet) {
		t.Fatalf("聚合数量不一致: got=%v want=%v", aggregated, expectedSet)
	}
	for _, modelID := range aggregated {
		if _, exists := expectedSet[modelID]; !exists {
			t.Errorf("聚合包含非预期模型 %s", modelID)
		}
	}
	if pool.BootstrapReadyCount() != 2 {
		t.Errorf("bootstrap 就绪计数应为 2,实际 %d", pool.BootstrapReadyCount())
	}
}

// TestPoolChangedBroadcast 验证租约生命周期触发变更广播。
// 注意语义:必须先持有通道引用再等待事件;事件发生后 Changed() 返回
// 重建的新通道。等待方因此必须在检查池状态之前获取引用。
func TestPoolChangedBroadcast(t *testing.T) {
	account := testAccount("a1", true, AccountReady, testChatModel("gemini-flash-latest"))
	pool := NewAccountPool([]*Account{account}, 2)

	lease, err := pool.AcquireFor(context.Background(), AccountSelection{})
	if err != nil {
		t.Fatalf("获取租约失败: %v", err)
	}
	changed := pool.Changed()
	select {
	case <-changed:
		t.Fatal("获取后通道不应立即关闭(应已重建)")
	default:
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("释放租约失败: %v", err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("释放租约后先前持有的通道应被关闭")
	}
}

// TestPoolConcurrentReadWrite 在读写并发下验证锁拆分安全性(-race 重点覆盖)。
func TestPoolConcurrentReadWrite(t *testing.T) {
	accounts := make([]*Account, 0, 8)
	for index := range 8 {
		accounts = append(accounts, testAccount(
			"account-"+string(rune('a'+index)), true, AccountReady,
			testChatModel("gemini-flash-latest"), testChatModel("model-"+string(rune('a'+index))),
		))
	}
	pool := NewAccountPool(accounts, 2)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 只读风暴:轻量视图 + 候选分类 + bootstrap 聚合
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				pool.AccountOverviews()
				pool.BootstrapModelIDs()
				pool.BootstrapReadyCount()
				_, _ = pool.ClassifyCandidates(context.Background(), AccountSelection{Method: "generateContent"}, nil)
				_ = pool.Changed()
			}
		}()
	}
	// 写风暴:租约获取与释放
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				lease, _, err := pool.TryAcquireFor(context.Background(), AccountSelection{})
				if err == nil && lease != nil {
					_ = lease.Release()
				}
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	if overviews := pool.AccountOverviews(); len(overviews) != 8 {
		t.Fatalf("并发后账户数应保持 8,实际 %d", len(overviews))
	}
}

// TestPoolChangedWakesWaiter 验证等待方在租约释放时立即唤醒(事件驱动调度)。
func TestPoolChangedWakesWaiter(t *testing.T) {
	account := testAccount("a1", true, AccountReady, testChatModel("gemini-flash-latest"))
	pool := NewAccountPool([]*Account{account}, 1)

	first, err := pool.AcquireFor(context.Background(), AccountSelection{})
	if err != nil {
		t.Fatalf("首个租约获取失败: %v", err)
	}
	woken := make(chan struct{})
	go func() {
		changed := pool.Changed()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-changed:
			close(woken)
		case <-timer.C:
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if err := first.Release(); err != nil {
		t.Fatalf("释放租约失败: %v", err)
	}
	select {
	case <-woken:
	case <-time.After(2 * time.Second):
		t.Fatal("等待方未在租约释放后及时唤醒")
	}
}

// benchmarkPool 构造 N 账户 × M 模型的池用于基准测试
func benchmarkPool(b *testing.B, accountCount int) *AccountPool {
	accounts := make([]*Account, 0, accountCount)
	for index := range accountCount {
		models := make([]Model, 0, 60)
		for modelIndex := range 60 {
			models = append(models, testChatModel(fmt.Sprintf("model-%03d", modelIndex)))
		}
		accounts = append(accounts, testAccount(
			fmt.Sprintf("account-%04d", index), true, AccountReady, models...,
		))
	}
	return NewAccountPool(accounts, 2)
}

// BenchmarkPoolStatus 基准:全量状态 DTO(每账户模型列表克隆+排序+冷却深拷贝)
func BenchmarkPoolStatus(b *testing.B) {
	pool := benchmarkPool(b, 64)
	b.ReportAllocs()
	for b.Loop() {
		_ = pool.Status()
	}
}

// BenchmarkPoolAccountOverviews 基准:轻量视图(零深拷贝)
func BenchmarkPoolAccountOverviews(b *testing.B) {
	pool := benchmarkPool(b, 64)
	b.ReportAllocs()
	for b.Loop() {
		_ = pool.AccountOverviews()
	}
}

// BenchmarkPrewarmPaths 对比预热目标计算路径
func BenchmarkPrewarmPaths(b *testing.B) {
	pool := benchmarkPool(b, 64)
	b.ReportAllocs()
	for b.Loop() {
		_ = pool.BootstrapReadyCount()
		_ = pool.BootstrapModelIDs()
	}
}

// TestBootstrapModelsAliasSemantics 验证 O(M) 重写后别名匹配语义
// 与原逐候选资格判定一致:资格作用于被匹配的实际模型,查询键可为别名。
func TestBootstrapModelsAliasSemantics(t *testing.T) {
	// model-a 合格,且带别名 alias-x;alias-x 本身作为另一个模型的 ID 存在但不合格
	eligible := testChatModel("model-a")
	eligible.CapabilityOptions = map[string][]string{"aliases": {"alias-x"}}
	ineligible := testChatModel("alias-x")
	ineligible.Capabilities = map[string]bool{}
	// preferred 别名挂在合格模型上
	preferredAlias := testChatModel("model-p")
	preferredAlias.CapabilityOptions = map[string][]string{"aliases": {"gemini-flash-latest"}}
	account := testAccount("a1", true, AccountReady, eligible, ineligible, preferredAlias)

	models := accountBootstrapModels(account)
	joined := map[string]int{}
	for index, modelID := range models {
		joined[modelID] = index
	}
	for _, expected := range []string{"gemini-flash-latest", "model-a", "alias-x", "model-p"} {
		if _, exists := joined[expected]; !exists {
			t.Errorf("应包含模型键 %s,实际 %v", expected, models)
		}
	}
	if joined["gemini-flash-latest"] != 0 {
		t.Errorf("preferred 模型应排在首位,实际 %v", models)
	}
	// 不合格模型的 ID 若同时是合格模型的别名,仍应保留(原语义)
	if _, exists := joined["model-a"]; !exists {
		t.Errorf("合格模型主 ID 应保留,实际 %v", models)
	}
}
