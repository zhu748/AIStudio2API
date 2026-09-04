package app

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// fakeAdminService 实现 api.AdminService,仅覆盖 failover 用到的
// Accounts 与 UpdateAccount 两个方法。
//
// 其余方法通过嵌入 nil 接口保持编译通过,意外调用会 panic 导致测试失败。
// 行为仿真与真实 AccountPool 一致:
//   - Accounts 返回当前账号摘要列表
//   - UpdateAccount 更新 Enabled 并同步 State(启用→ready,禁用→disabled),
//     与 AccountPool.Status() 对 Enabled=false 恒报 disabled 的行为一致
type fakeAdminService struct {
	api.AdminService

	mu       sync.Mutex
	accounts []api.AdminAccount
	updates  []fakeUpdate
	failOn   map[string]error // 按账号 ID 注入 UpdateAccount 错误
}

type fakeUpdate struct {
	id      string
	enabled bool
}

func (f *fakeAdminService) Accounts(ctx context.Context) ([]api.AdminAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]api.AdminAccount, len(f.accounts))
	copy(out, f.accounts)
	return out, nil
}

func (f *fakeAdminService) UpdateAccount(ctx context.Context, id string, input api.AccountInput) (api.AdminAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn != nil {
		if err, ok := f.failOn[id]; ok {
			return api.AdminAccount{}, err
		}
	}
	for i := range f.accounts {
		if f.accounts[i].ID != id {
			continue
		}
		f.accounts[i].Enabled = input.Enabled
		f.accounts[i].Label = input.Label
		f.accounts[i].Proxy = input.Proxy
		f.accounts[i].Locale = input.Locale
		f.accounts[i].Timezone = input.Timezone
		if input.Enabled {
			f.accounts[i].State = string(aistudio.AccountReady)
		} else {
			f.accounts[i].State = string(aistudio.AccountDisabled)
		}
		f.updates = append(f.updates, fakeUpdate{id: id, enabled: input.Enabled})
		return f.accounts[i], nil
	}
	return api.AdminAccount{}, errors.New("account not found: " + id)
}

// setState 模拟运行时将账号标记为失效(如凭证过期)
func (f *fakeAdminService) setState(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.accounts {
		if f.accounts[i].ID == id {
			f.accounts[i].State = state
		}
	}
}

func (f *fakeAdminService) enabled(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, acc := range f.accounts {
		if acc.ID == id {
			return acc.Enabled
		}
	}
	return false
}

// newTestMonitor 构造挂接 fake 管理端的 failover 监控器
func newTestMonitor(accounts []api.AdminAccount) (*failoverMonitor, *fakeAdminService) {
	fake := &fakeAdminService{accounts: accounts}
	manager := &runtimeManager{current: &runtimeGeneration{admin: fake}}
	return &failoverMonitor{manager: manager, exhausted: make(map[string]bool)}, fake
}

func testAccount(id string, state string, enabled bool) api.AdminAccount {
	return api.AdminAccount{
		ID:       id,
		Label:    "label-" + id,
		Enabled:  enabled,
		State:    state,
		Proxy:    "http://proxy-" + id + ":8080",
		Locale:   "en-US",
		Timezone: "Asia/Shanghai",
	}
}

// 场景: active 账号失效 → 禁用失效账号并启用第一个健康备用
func TestFailoverSwitchesToHealthyBackup(t *testing.T) {
	monitor, fake := newTestMonitor([]api.AdminAccount{
		testAccount("a", string(aistudio.AccountAuthRequired), true),
		testAccount("b", string(aistudio.AccountDisabled), false),
		testAccount("c", string(aistudio.AccountDisabled), false),
	})

	if err := monitor.checkAndFailover(); err != nil {
		t.Fatalf("failover 应成功: %v", err)
	}

	if fake.enabled("a") {
		t.Error("失效账号 a 应被禁用")
	}
	if !fake.enabled("b") {
		t.Error("应启用备用账号 b(第一个健康候选)")
	}
	if fake.enabled("c") {
		t.Error("不应启用备用账号 c(轮询顺序,b 优先)")
	}
	if !monitor.exhausted["a"] {
		t.Error("失效账号 a 应被拉黑")
	}
	if len(fake.updates) != 2 {
		t.Fatalf("期望恰好 2 次 UpdateAccount(禁用 a + 启用 b),实际 %d 次", len(fake.updates))
	}
	// 断言顺序: 先禁用失效账号,再启用备用(若顺序颠倒,中途失败会留下零可用状态)
	if fake.updates[0].id != "a" || fake.updates[0].enabled {
		t.Errorf("第一次更新应是对 a 的禁用,实际 %+v", fake.updates[0])
	}
	if fake.updates[1].id != "b" || !fake.updates[1].enabled {
		t.Errorf("第二次更新应是对 b 的启用,实际 %+v", fake.updates[1])
	}
}

// 场景: 备用账号也失效 → 拉黑防震荡,切换到下一个候选;全部失效后不回切
func TestFailoverDoesNotReEnableExhaustedAccount(t *testing.T) {
	monitor, fake := newTestMonitor([]api.AdminAccount{
		testAccount("a", string(aistudio.AccountAuthRequired), true),
		testAccount("b", string(aistudio.AccountDisabled), false),
		testAccount("c", string(aistudio.AccountDisabled), false),
	})

	// 第一轮: a 失效 → 禁用 a,启用 b
	if err := monitor.checkAndFailover(); err != nil {
		t.Fatalf("第一轮 failover 应成功: %v", err)
	}

	// 模拟 b 启用后凭证也过期(运行时将其标记为 auth_required)
	fake.setState("b", string(aistudio.AccountAuthRequired))

	// 第二轮: b 失效 → 应切换到 c,而不是回到 a(a 在黑名单)
	if err := monitor.checkAndFailover(); err != nil {
		t.Fatalf("第二轮 failover 应成功: %v", err)
	}
	if fake.enabled("a") {
		t.Error("已拉黑的失效账号 a 不得被重新启用(防 A<->B 震荡)")
	}
	if fake.enabled("b") {
		t.Error("失效账号 b 应被禁用")
	}
	if !fake.enabled("c") {
		t.Error("应切换到账号 c")
	}

	// 第三轮: c 也失效 → a/b 均在黑名单,无可用备用 → 不动作,c 保持启用
	fake.setState("c", string(aistudio.AccountAuthRequired))
	if err := monitor.checkAndFailover(); err != nil {
		t.Fatalf("第三轮 failover 应成功: %v", err)
	}
	if fake.enabled("a") || fake.enabled("b") {
		t.Error("全部备用失效后不得回切已拉黑账号")
	}
	if !fake.enabled("c") {
		t.Error("无备用时当前失效账号应保持启用(管理端可见失效原因,而非零可用)")
	}
}

// 场景: 摘要中已标失效的禁用账号不可作为候选(排除已确认失效的凭证)
func TestFailoverSkipsKnownBadBackup(t *testing.T) {
	monitor, fake := newTestMonitor([]api.AdminAccount{
		testAccount("a", string(aistudio.AccountAuthRequired), true),
		testAccount("bad", string(aistudio.AccountAuthRequired), false),
		testAccount("good", string(aistudio.AccountDisabled), false),
	})

	if err := monitor.checkAndFailover(); err != nil {
		t.Fatalf("failover 应成功: %v", err)
	}

	if fake.enabled("bad") {
		t.Error("摘要 State 为 auth_required 的禁用账号应被跳过")
	}
	if !fake.enabled("good") {
		t.Error("应选择健康候选 good")
	}
}

// 场景: 无备用账号 → 只告警不动作,不报错
func TestFailoverNoBackupIsNoop(t *testing.T) {
	monitor, fake := newTestMonitor([]api.AdminAccount{
		testAccount("a", string(aistudio.AccountAuthRequired), true),
	})

	if err := monitor.checkAndFailover(); err != nil {
		t.Fatalf("无备用时应安静返回: %v", err)
	}
	if len(fake.updates) != 0 {
		t.Errorf("无备用时不应有任何 UpdateAccount 调用,实际 %d 次", len(fake.updates))
	}
}

// 场景: 所有账号健康 → 不动作
func TestFailoverHealthyActiveIsNoop(t *testing.T) {
	monitor, fake := newTestMonitor([]api.AdminAccount{
		testAccount("a", string(aistudio.AccountReady), true),
		testAccount("b", string(aistudio.AccountDisabled), false),
	})

	if err := monitor.checkAndFailover(); err != nil {
		t.Fatalf("健康状态应安静返回: %v", err)
	}
	if len(fake.updates) != 0 {
		t.Errorf("健康状态不应有任何 UpdateAccount 调用,实际 %d 次", len(fake.updates))
	}
}

// 场景: 禁用失效账号失败 → 不拉黑、不启用备用(下一轮重试禁用)
func TestFailoverDisableErrorKeepsAccountEnabled(t *testing.T) {
	monitor, fake := newTestMonitor([]api.AdminAccount{
		testAccount("a", string(aistudio.AccountAuthRequired), true),
		testAccount("b", string(aistudio.AccountDisabled), false),
	})
	fake.failOn = map[string]error{"a": errors.New("disable boom")}

	if err := monitor.checkAndFailover(); err == nil {
		t.Fatal("禁用失败时应返回错误")
	}
	if monitor.exhausted["a"] {
		t.Error("禁用失败的账号不应进入黑名单(仍启用,下一轮重试)")
	}
	if len(fake.updates) != 0 {
		t.Errorf("禁用失败时不应继续启用备用,实际 %d 次调用", len(fake.updates))
	}
	if !fake.enabled("a") {
		t.Error("禁用失败时账号 a 应保持启用")
	}
}

// 场景: 切换中断(禁用成功、启用备用失败)→ 所有账号被禁用
// 自愈路径应在下一轮重新启用健康候选,而不是卡死在零可用状态
func TestFailoverRecoversWhenAllDisabledMidSwitch(t *testing.T) {
	monitor, fake := newTestMonitor([]api.AdminAccount{
		testAccount("a", string(aistudio.AccountAuthRequired), true),
		testAccount("b", string(aistudio.AccountDisabled), false),
	})
	fake.failOn = map[string]error{"b": errors.New("enable boom")}

	// 第一次: 禁用 a 成功,启用 b 失败 → 零可用状态
	if err := monitor.checkAndFailover(); err == nil {
		t.Fatal("启用备用失败时应返回错误")
	}
	if !monitor.exhausted["a"] {
		t.Error("禁用成功的失效账号应被拉黑")
	}
	if fake.enabled("a") || fake.enabled("b") {
		t.Fatal("中断后应无任何启用账号")
	}

	// 第二次: 零启用 + 存在拉黑记录 → 自愈重新启用 b
	fake.failOn = nil
	if err := monitor.checkAndFailover(); err != nil {
		t.Fatalf("自愈路径不应报错: %v", err)
	}
	if !fake.enabled("b") {
		t.Error("自愈应重新启用账号 b(未拉黑的第一个健康候选)")
	}
	if fake.enabled("a") {
		t.Error("自愈不得启用已拉黑的账号 a")
	}
}

// 场景: 运维主动停用全部账号(无拉黑记录)→ 自愈不得干预
func TestFailoverDoesNotRecoverOperatorDisabledAll(t *testing.T) {
	monitor, fake := newTestMonitor([]api.AdminAccount{
		testAccount("a", string(aistudio.AccountDisabled), false),
		testAccount("b", string(aistudio.AccountDisabled), false),
	})

	if err := monitor.checkAndFailover(); err != nil {
		t.Fatalf("应安静返回: %v", err)
	}
	if len(fake.updates) != 0 {
		t.Error("无拉黑记录时视为运维主动停用,不应触发自愈")
	}
}

// accountInputFromAdmin 必须保留账号的配置字段,否则切换会静默丢失
// Proxy/Locale/Timezone(UpdateAccount 以输入为准整体覆盖)
func TestAccountInputFromAdminPreservesFields(t *testing.T) {
	accounts := []api.AdminAccount{
		testAccount("a", string(aistudio.AccountAuthRequired), true),
	}

	input := accountInputFromAdmin(accounts, "a", false)
	if input.Label != "label-a" || input.Enabled {
		t.Errorf("字段不匹配: %+v", input)
	}
	if input.Proxy != "http://proxy-a:8080" {
		t.Errorf("Proxy 应保留: %q", input.Proxy)
	}
	if input.Locale != "en-US" || input.Timezone != "Asia/Shanghai" {
		t.Errorf("Locale/Timezone 应保留: %+v", input)
	}

	// 未命中 id 时的回退(不应 panic)
	fallback := accountInputFromAdmin(accounts, "missing", true)
	if fallback.Label != "missing" || !fallback.Enabled {
		t.Errorf("回退输入不匹配: %+v", fallback)
	}
}
