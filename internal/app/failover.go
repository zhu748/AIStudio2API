package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// failoverMonitor 后台监控 active 账号,失效时自动切换到健康备用账号
//
// 仅在 AISTUDIO_FAILOVER=true 且管理端可用时启动
// 不侵入核心调度逻辑,通过调用现有 UpdateAccount API 实现
type failoverMonitor struct {
	manager *runtimeManager
	// exhausted 记录本进程内已确认失效并被禁用的账号 ID,
	// 防止失效账号被反复启用造成 A<->B 震荡切换。
	// 进程重启后清零(环境变量重新注入,状态重新评估)。
	exhausted map[string]bool
}

// startFailoverMonitor 启动后台监控,当 active 账号变为 auth_required/unavailable 时
// 自动禁用当前账号并启用列表中下一个健康备用账号
func startFailoverMonitor(ctx context.Context, manager *runtimeManager) {
	if !failoverEnabled() {
		return
	}
	if manager == nil {
		return
	}
	monitor := &failoverMonitor{manager: manager, exhausted: make(map[string]bool)}
	slog.Info("账号失效自动切换已启用", "check_interval", "30s")
	go monitor.loop(ctx)
}

// loop 周期执行 failover 检查
func (m *failoverMonitor) loop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.checkAndFailover(); err != nil {
				slog.Warn("failover 检查失败", "error", err)
			}
		}
	}
}

// checkAndFailover 执行一次 failover 检查
//
// 步骤:
//  1. 获取账号列表
//  2. 找到当前 enabled 且 state=auth_required/unavailable 的账号
//  3. 在 enabled=false 且未拉黑的账号中找一个健康候选
//  4. 通过 UpdateAccount 切换: 禁用旧的并拉黑,启用新的
//  5. 自愈: 若切换中断导致所有账号都被禁用,重试启用健康候选
func (m *failoverMonitor) checkAndFailover() error {
	current := m.current()
	if current == nil || current.admin == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	accounts, err := current.admin.Accounts(ctx)
	if err != nil {
		return fmt.Errorf("获取账号列表: %w", err)
	}

	// 找到失效的 active 账号,同时记录是否存在任何启用账号
	var failedID string
	anyEnabled := false
	for _, acc := range accounts {
		if !acc.Enabled {
			continue
		}
		anyEnabled = true
		state := strings.TrimSpace(acc.State)
		if state == string(aistudio.AccountAuthRequired) ||
			state == string(aistudio.AccountUnavailable) {
			failedID = acc.ID
			break
		}
	}

	if failedID == "" {
		// 自愈路径: 本进程执行过切换(存在拉黑记录)但当前没有任何
		// 启用账号——说明上次切换在"禁用旧账号"后、"启用备用"前中断
		// (如管理端瞬时错误)。此时重试启用一个健康候选,避免服务
		// 停留在零可用状态直到重启。
		// 拉黑记录为空时视为运维主动停用全部账号,不干预。
		if !anyEnabled && len(m.exhausted) > 0 {
			return m.recoverEnabled(ctx, current, accounts)
		}
		return nil
	}

	// 找一个健康的备用账号(排除已拉黑与已确认失效的,见 pickBackup)
	backupID, ok := m.pickBackup(accounts)
	if !ok {
		slog.Warn("failover 无可用备用账号",
			"failed", failedID,
			"hint", "所有备用账号均已失效,请更新 AISTUDIO_AUTH_ACCOUNTS 后重启",
		)
		return nil
	}

	slog.Info("触发自动账号切换",
		"from", failedID,
		"to", backupID,
	)

	// 禁用失效账号(失败时不拉黑,账号仍启用,下一轮重试禁用)
	if _, err := current.admin.UpdateAccount(ctx, failedID, accountInputFromAdmin(accounts, failedID, false)); err != nil {
		return fmt.Errorf("禁用失效账号 %s: %w", failedID, err)
	}
	// 拉黑失效账号,后续轮次不再将其选为候选
	m.exhausted[failedID] = true
	// 启用备用账号(失败时由下一轮自愈路径重试)
	if _, err := current.admin.UpdateAccount(ctx, backupID, accountInputFromAdmin(accounts, backupID, true)); err != nil {
		return fmt.Errorf("启用备用账号 %s: %w", backupID, err)
	}

	slog.Info("自动账号切换完成",
		"from", failedID,
		"to", backupID,
	)
	return nil
}

// current 返回当前运行时快照
func (m *failoverMonitor) current() *runtimeGeneration {
	m.manager.mu.RLock()
	defer m.manager.mu.RUnlock()
	return m.manager.current
}

// pickBackup 在未启用的账号中选择下一个候选
//
// 注意: AccountPool.Status() 对 Enabled=false 的账号统一报告 State=disabled
// (见 accounts.go Status() 与 initialAccountState),因此不能要求备用账号
// 状态为 ready——那永远匹配不到。正确的判定是排除已确认失效的账号:
//  1. 摘要 State 为 auth_required/unavailable 的跳过(凭证已确认失效)
//  2. 本进程已拉黑(exhausted)的跳过(禁用动作会把失效状态覆盖为 disabled,
//     摘要上无法再区分,必须靠内存黑名单防止反复启用同一失效账号)
//
// 若候选账号凭证实际已过期,启用后运行时会将其重新标记为 auth_required,
// 下一轮 failover(30s 后)将其拉黑并继续切换,最终收敛到健康账号。
func (m *failoverMonitor) pickBackup(accounts []api.AdminAccount) (string, bool) {
	for _, acc := range accounts {
		if acc.Enabled {
			continue
		}
		if m.exhausted[acc.ID] {
			continue
		}
		state := strings.TrimSpace(acc.State)
		if state == string(aistudio.AccountAuthRequired) ||
			state == string(aistudio.AccountUnavailable) {
			continue
		}
		return acc.ID, true
	}
	return "", false
}

// recoverEnabled 自愈路径: 服务处于零可用状态时重新启用一个健康候选
func (m *failoverMonitor) recoverEnabled(ctx context.Context, current *runtimeGeneration, accounts []api.AdminAccount) error {
	backupID, ok := m.pickBackup(accounts)
	if !ok {
		slog.Warn("failover 自愈失败: 无可用备用账号",
			"hint", "所有账号均已失效,请更新 AISTUDIO_AUTH_ACCOUNTS 后重启",
		)
		return nil
	}
	if _, err := current.admin.UpdateAccount(ctx, backupID, accountInputFromAdmin(accounts, backupID, true)); err != nil {
		return fmt.Errorf("自愈启用账号 %s: %w", backupID, err)
	}
	slog.Info("failover 自愈: 重新启用账号", "account", backupID)
	return nil
}

// accountInputFromAdmin 从管理端账号摘要构造 UpdateAccount 输入
func accountInputFromAdmin(accounts []api.AdminAccount, id string, enabled bool) api.AccountInput {
	for _, acc := range accounts {
		if acc.ID == id {
			return api.AccountInput{
				Label:    acc.Label,
				Enabled:  enabled,
				Proxy:    acc.Proxy,
				Locale:   acc.Locale,
				Timezone: acc.Timezone,
			}
		}
	}
	return api.AccountInput{Label: id, Enabled: enabled}
}
