// Package sidecar 用 sing-box 子进程承载 proxyproto 解析出的
// "第 2 级" 代理协议(vmess / vless REALITY / hysteria2 / tuic / SS2022 等),
// 在 127.0.0.1 随机端口暴露一个本地 SOCKS5 入口。
//
// 工作方式:Ensure(分享链接) 按链接原文去重,首次遇到时把
// proxyproto.SidecarNode 的出站 JSON 套上 socks 入站模板写成临时
// 配置文件,拉起 `sing-box run` 子进程并等待端口就绪;再次 Ensure
// 同一链接直接复用已存活的进程,进程崩溃后按需自动重建。
//
// 二进制发现顺序:SINGBOX_PATH 环境变量 → /usr/local/bin/sing-box
// (Docker 镜像预置)→ PATH → 用户缓存目录(可选自动下载,带 sha256
// 校验,见 binary.go)。
//
// 生命周期:Linux 下子进程带 Pdeathsig,主进程退出自动清理;
// Shutdown 供优雅停机时主动回收。进程不与单次登录会话绑定
// (与进程内 SOCKS5 桥不同),转换层是全进程共享的基础设施。
package sidecar

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/proxyproto"
)

// readyWaitTimeout 是等待 sing-box 端口就绪的上限
const readyWaitTimeout = 20 * time.Second

// sidecarProcess 描述一个运行中的 sing-box 转换层进程
type sidecarProcess struct {
	raw     string
	node    *proxyproto.SidecarNode
	port    int
	cmd     *exec.Cmd
	exited  chan struct{}
	stderr  *logBuffer
	cfgPath string

	mu sync.Mutex
}

// socksURL 返回该进程的本地 SOCKS5 地址
func (p *sidecarProcess) socksURL() string {
	return fmt.Sprintf("socks5://127.0.0.1:%d", p.port)
}

// alive 报告进程是否仍在运行(cmd.Wait 返回后 exited 关闭)
func (p *sidecarProcess) alive() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

var (
	registryMu sync.Mutex
	registry   = make(map[string]*sidecarProcess)
	// pending 记录正在启动中的链接:同一链接并发 Ensure 只允许
	// 一个启动者,其余调用方等待其完成后直接复用结果,避免竞态下
	// 拉起重复的 sing-box 进程(先启动者被覆盖后无人回收,泄漏到进程退出)。
	pending = make(map[string]chan struct{})
)

// Ensure 返回分享链接对应的本地 SOCKS5 代理地址。
//
// 行为:
//   - 标准代理(http/socks 系)与原生可拨号链接原样返回,由调用方
//     走既有链路(进程内桥或直接拨号)
//   - 需要 sing-box 承载的链接按原文去重拉起子进程,返回
//     socks5://127.0.0.1:<port>
//
// 进程已死时自动重建(端口可能变化);启动失败重试 3 次
// (覆盖本地端口竞争窗口)。同一链接的并发启动由 pending
// 单飞互斥,等待方在启动者完成后复用同一进程。
func Ensure(raw string) (string, error) {
	outbound, err := proxyproto.Parse(raw)
	if err != nil {
		return "", err
	}
	if !outbound.NeedsSidecar() {
		// 标准代理或原生链接:调用方负责,这里原样透传
		return raw, nil
	}
	key := outbound.Raw()
	if process := lookupAliveSidecar(key); process != nil {
		return sidecarAddress(process), nil
	}
	// 单飞:已有并发调用方在启动同一链接,等它完成后直接复用
	registryMu.Lock()
	if done, exists := pending[key]; exists {
		registryMu.Unlock()
		<-done
		if process := lookupAliveSidecar(key); process != nil {
			return sidecarAddress(process), nil
		}
		return "", fmt.Errorf("sing-box 并发启动未成功(%s),请重试", outbound.Sidecar().Type)
	}
	done := make(chan struct{})
	pending[key] = done
	registryMu.Unlock()

	var (
		process  *sidecarProcess
		startErr error
	)
	for attempt := 0; attempt < 3 && process == nil; attempt++ {
		process, startErr = startProcess(key, outbound.Sidecar())
	}
	registryMu.Lock()
	delete(pending, key)
	if process != nil {
		registry[key] = process
	}
	close(done)
	registryMu.Unlock()
	if process == nil {
		return "", fmt.Errorf("sing-box 转换层启动失败(%s): %w", outbound.Sidecar().Type, startErr)
	}
	return sidecarAddress(process), nil
}

// lookupAliveSidecar 返回 key 对应的存活进程;进程已死时
// 从注册表移除、清理配置文件并记录诊断日志,返回 nil。
func lookupAliveSidecar(key string) *sidecarProcess {
	registryMu.Lock()
	process := registry[key]
	registryMu.Unlock()
	if process == nil {
		return nil
	}
	process.mu.Lock()
	alive := process.alive()
	process.mu.Unlock()
	if alive {
		return process
	}
	// 已死:清理并移除注册表,走新建路径
	registryMu.Lock()
	if registry[key] == process {
		delete(registry, key)
	}
	registryMu.Unlock()
	process.mu.Lock()
	process.stopLocked()
	process.mu.Unlock()
	slog.Warn("sing-box 转换层进程已退出,下次 Ensure 将重建",
		"protocol", process.node.Type, "node", process.node.Tag, "stderr", process.stderr.tail(2048))
	return nil
}

// sidecarAddress 在进程锁内读取本地 SOCKS5 地址。
func sidecarAddress(process *sidecarProcess) string {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.socksURL()
}

// startProcess 拉起一个新的 sing-box 子进程并等待就绪
func startProcess(raw string, node *proxyproto.SidecarNode) (*sidecarProcess, error) {
	process := &sidecarProcess{
		raw:    raw,
		node:   node,
		stderr: newLogBuffer(32 * 1024),
		exited: make(chan struct{}),
	}
	process.mu.Lock()
	defer process.mu.Unlock()
	if err := process.startLocked(); err != nil {
		return nil, err
	}
	if err := process.waitReadyLocked(); err != nil {
		if process.cmd != nil && process.cmd.Process != nil {
			_ = process.cmd.Process.Kill()
		}
		process.stopLocked()
		return nil, err
	}
	slog.Info("sing-box 转换层已启动",
		"protocol", node.Type, "node", node.Tag, "socks", process.socksURL())
	return process, nil
}

// startLocked 选择端口、写配置并启动子进程(须持锁调用)
func (p *sidecarProcess) startLocked() error {
	binary, err := resolveBinary(true)
	if err != nil {
		return err
	}
	port, err := pickFreePort()
	if err != nil {
		return err
	}
	p.port = port
	cfgPath, err := writeConfigFile(p.node, port)
	if err != nil {
		return err
	}
	p.cfgPath = cfgPath

	cmd := exec.Command(binary, "run", "--disable-color", "-c", cfgPath)
	applySysProcAttr(cmd)
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = os.Remove(cfgPath)
		return fmt.Errorf("sing-box stderr 管道创建失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = os.Remove(cfgPath)
		return fmt.Errorf("sing-box 启动失败(%s): %w", binary, err)
	}
	p.cmd = cmd
	go drainStderr(stderrPipe, p.stderr)
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	return nil
}

// waitReadyLocked 轮询本地端口直至 sing-box 就绪(须持锁调用)
func (p *sidecarProcess) waitReadyLocked() error {
	deadline := time.Now().Add(readyWaitTimeout)
	address := fmt.Sprintf("127.0.0.1:%d", p.port)
	for time.Now().Before(deadline) {
		if !p.alive() {
			return fmt.Errorf("sing-box 进程提前退出(%s): %s", p.node.Type, p.stderr.tail(2048))
		}
		conn, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("sing-box 端口就绪超时(%s): %s", p.node.Type, p.stderr.tail(2048))
}

// stopLocked 清理配置文件(须持锁调用,进程 kill 由调用方处理)
func (p *sidecarProcess) stopLocked() {
	if p.cfgPath != "" {
		_ = os.Remove(p.cfgPath)
		p.cfgPath = ""
	}
}

// Shutdown 停止全部转换层进程(应用退出时调用)
func Shutdown() {
	registryMu.Lock()
	processes := make([]*sidecarProcess, 0, len(registry))
	for _, process := range registry {
		processes = append(processes, process)
	}
	registry = make(map[string]*sidecarProcess)
	registryMu.Unlock()
	for _, process := range processes {
		process.mu.Lock()
		if process.cmd != nil && process.cmd.Process != nil {
			_ = process.cmd.Process.Kill()
		}
		process.stopLocked()
		process.mu.Unlock()
	}
}

// pickFreePort 选取一个空闲的本地 TCP 端口
// (监听后立即释放再交给 sing-box,存在极小的端口竞争窗口,
//
//	失败时由上层 Ensure 重试)
func pickFreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("本地端口分配失败: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port, nil
}

// configDocument 是 sing-box 配置文件结构
type configDocument struct {
	Log       map[string]any   `json:"log"`
	Inbounds  []map[string]any `json:"inbounds"`
	Outbounds []map[string]any `json:"outbounds"`
	Route     map[string]any   `json:"route"`
}

// writeConfigFile 生成 sing-box 配置并写入临时目录
func writeConfigFile(node *proxyproto.SidecarNode, port int) (string, error) {
	outbound := make(map[string]any, len(node.Outbound)+1)
	for key, value := range node.Outbound {
		outbound[key] = value
	}
	outbound["tag"] = "proxy"
	document := configDocument{
		Log:       map[string]any{"level": "warn", "timestamp": true},
		Inbounds:  []map[string]any{{"type": "socks", "tag": "bridge-in", "listen": "127.0.0.1", "listen_port": port}},
		Outbounds: []map[string]any{outbound},
		Route:     map[string]any{"final": "proxy"},
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", fmt.Errorf("生成 sing-box 配置失败: %w", err)
	}
	path := filepath.Join(os.TempDir(), fmt.Sprintf("aistudio2api-singbox-%s-%d.json", node.Type, port))
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return "", fmt.Errorf("写入 sing-box 配置失败: %w", err)
	}
	return path, nil
}

// drainStderr 把 sing-box stderr 写入环形缓冲
func drainStderr(pipe io.Reader, buffer *logBuffer) {
	chunk := make([]byte, 4096)
	for {
		n, err := pipe.Read(chunk)
		if n > 0 {
			buffer.write(chunk[:n])
		}
		if err != nil {
			return
		}
	}
}
