package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// startFailoverMonitor 启动后台监控,当 active 账号变为 auth_required 时
// 自动禁用当前账号并启用列表中下一个 ready 账号
//
// 仅在 AISTUDIO_FAILOVER=true 且管理端可用时启动
// 不侵入核心调度逻辑,通过调用现有 UpdateAccount API 实现
func startFailoverMonitor(ctx context.Context, manager *runtimeManager) {
	if !failoverEnabled() {
		return
	}
	if manager == nil {
		return
	}
	slog.Info("账号失效自动切换已启用", "check_interval", "30s")
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := checkAndFailover(manager); err != nil {
					slog.Warn("failover 检查失败", "error", err)
				}
			}
		}
	}()
}

// checkAndFailover 执行一次 failover 检查
//
// 步骤:
//  1. 获取账号列表
//  2. 找到当前 enabled 且 state=auth_required/unavailable 的账号
//  3. 在 enabled=false 的账号中找一个 ready 的
//  4. 通过 UpdateAccount 切换: 禁用旧的,启用新的
func checkAndFailover(manager *runtimeManager) error {
	manager.mu.RLock()
	current := manager.current
	manager.mu.RUnlock()
	if current == nil || current.admin == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	accounts, err := current.admin.Accounts(ctx)
	if err != nil {
		return fmt.Errorf("获取账号列表: %w", err)
	}

	// 找到失效的 active 账号
	var failedID string
	for _, acc := range accounts {
		if !acc.Enabled {
			continue
		}
		state := strings.TrimSpace(acc.State)
		if state == string(aistudio.AccountAuthRequired) ||
			state == string(aistudio.AccountUnavailable) {
			failedID = acc.ID
			break
		}
	}
	if failedID == "" {
		return nil
	}

	// 找一个 ready 的备用账号
	var backupID string
	for _, acc := range accounts {
		if acc.Enabled {
			continue
		}
		if strings.TrimSpace(acc.State) == string(aistudio.AccountReady) {
			backupID = acc.ID
			break
		}
	}
	if backupID == "" {
		slog.Warn("failover 无可用备用账号",
			"failed", failedID,
			"hint", "请在管理界面手动登录或更新凭证后重启",
		)
		return nil
	}

	slog.Info("触发自动账号切换",
		"from", failedID,
		"to", backupID,
	)

	// 禁用失效账号
	if _, err := current.admin.UpdateAccount(ctx, failedID, accountInputFromAdmin(accounts, failedID, false)); err != nil {
		return fmt.Errorf("禁用失效账号 %s: %w", failedID, err)
	}
	// 启用备用账号
	if _, err := current.admin.UpdateAccount(ctx, backupID, accountInputFromAdmin(accounts, backupID, true)); err != nil {
		return fmt.Errorf("启用备用账号 %s: %w", backupID, err)
	}

	slog.Info("自动账号切换完成",
		"from", failedID,
		"to", backupID,
	)
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
