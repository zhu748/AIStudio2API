package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyproto"
)

// NativeLoginDriver 通过纯 Go WebDriver BiDi 完成隔离登录
type NativeLoginDriver struct {
	camoufox string
	timeout  time.Duration
}

var _ IsolatedLoginDriver = (*NativeLoginDriver)(nil)

// NewNativeLoginDriver 创建纯 Go Camoufox 登录驱动
func NewNativeLoginDriver(camoufoxPath string, timeout time.Duration) (*NativeLoginDriver, error) {
	camoufoxPath = strings.TrimSpace(camoufoxPath)
	if camoufoxPath == "" {
		return nil, errors.New("缺少 Camoufox 路径")
	}
	absolute, err := filepath.Abs(camoufoxPath)
	if err != nil {
		return nil, fmt.Errorf("解析 Camoufox 路径: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, fmt.Errorf("读取 Camoufox: %w", err)
	}
	if info.IsDir() {
		return nil, errors.New("Camoufox 路径是目录")
	}
	if timeout <= 0 {
		return nil, errors.New("Camoufox 登录超时必须为正数")
	}
	return &NativeLoginDriver{camoufox: absolute, timeout: timeout}, nil
}

// Login 启动可见隔离 Camoufox 并导出认证状态
func (driver *NativeLoginDriver) Login(ctx context.Context, request IsolatedLoginRequest) (IsolatedLoginResult, error) {
	if driver == nil {
		return IsolatedLoginResult{}, errors.New("纯 Go Camoufox 登录驱动未初始化")
	}
	result, err := camoufoxnative.Login(ctx, driver.options(ctx, request))
	if err != nil {
		return IsolatedLoginResult{}, err
	}
	var state StorageState
	if err := json.Unmarshal(result.StorageStateJSON, &state); err != nil {
		return IsolatedLoginResult{}, fmt.Errorf("解析隔离登录状态: %w", err)
	}
	if err := state.Validate(); err != nil {
		return IsolatedLoginResult{}, err
	}
	if err := state.SetAuthExtension(AuthExtension{
		Source: AuthSource{Browser: "camoufox", Email: result.Email},
	}); err != nil {
		return IsolatedLoginResult{}, err
	}
	return IsolatedLoginResult{StorageState: state, Email: result.Email, VerifiedAt: result.VerifiedAt}, nil
}

// Verify 使用无头隔离 Camoufox 验证已有认证状态
func (driver *NativeLoginDriver) Verify(ctx context.Context, request IsolatedLoginRequest, state StorageState) (LoginVerification, error) {
	if driver == nil {
		return LoginVerification{}, errors.New("纯 Go Camoufox 登录驱动未初始化")
	}
	if err := state.Validate(); err != nil {
		return LoginVerification{}, err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return LoginVerification{}, fmt.Errorf("编码隔离验证状态: %w", err)
	}
	verification, err := camoufoxnative.Verify(ctx, driver.options(ctx, request), encoded)
	if err != nil {
		return LoginVerification{}, err
	}
	return LoginVerification{
		Authenticated: verification.Authenticated,
		VerifiedAt:    verification.VerifiedAt,
		Reason:        verification.Reason,
	}, nil
}

func (driver *NativeLoginDriver) options(ctx context.Context, request IsolatedLoginRequest) camoufoxnative.LoginOptions {
	return camoufoxnative.LoginOptions{
		ExecutablePath: driver.camoufox,
		Directory:      request.Directory,
		Locale:         request.Locale,
		Timezone:       request.Timezone,
		Proxy:          browserLoginProxy(ctx, request.Proxy),
		Timeout:        driver.timeout,
	}
}

// browserLoginProxy 把登录代理转换为 Camoufox 可用形式:
// vless/trojan/ss 分享链接在登录会话内架设 SOCKS5 桥(会话结束自动关闭),
// 其余(http/https/socks5/空)原样返回。
func browserLoginProxy(ctx context.Context, proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return ""
	}
	outbound, err := proxyproto.Parse(proxyURL)
	if err != nil || outbound.IsStandard() {
		return proxyURL
	}
	bridged, err := proxyproto.ServeSOCKS5(ctx, outbound)
	if err != nil {
		// 建桥失败时回退原 URL,由 Camoufox 报出明确错误
		return proxyURL
	}
	return bridged
}
