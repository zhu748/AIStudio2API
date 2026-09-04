package sidecar

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

// 测试固定凭证
const (
	testUUID        = "b831381d-6324-4d53-ad4f-8cda48b30811"
	testSS2022Key   = "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=" // 32 字节 base64
	testTrojanPass  = "passw0rd"
	testHy2Password = "hy2pw"
	testHy2ObfsPW   = "obfpw"
	testTUICPass    = "tuicpw"
)

// echoTestCase 描述一条端到端链路
type echoTestCase struct {
	name string
	link string
}

// TestSidecarEndToEnd 拉起真实 sing-box 服务端(含 9 种协议 inbound),
// 逐协议验证 Ensure → 本地 socks5 → 回声服务器 全链路。
// 未安装 sing-box 二进制时跳过(不触发下载)。
func TestSidecarEndToEnd(t *testing.T) {
	binary, err := resolveBinary(false)
	if err != nil {
		t.Skipf("未找到 sing-box 二进制,跳过端到端测试: %v", err)
	}
	t.Cleanup(Shutdown)

	echoAddress := startEchoServer(t)
	certPath, keyPath := writeSelfSignedCert(t)

	ports := map[string]int{
		"vless-vision": 0, "vless-grpc": 0, "vless-httpupgrade": 0, "vless-ws-ed": 0,
		"vmess-tcp": 0, "vmess-ws": 0,
		"trojan-grpc": 0, "ss2022": 0, "hy2": 0, "tuic": 0,
	}
	for name := range ports {
		port, portErr := pickFreePort()
		if portErr != nil {
			t.Fatalf("分配端口失败: %v", portErr)
		}
		ports[name] = port
	}
	serverConfig := buildTestServerConfig(t, ports, certPath, keyPath)
	serverPath := filepath.Join(t.TempDir(), "server.json")
	serverJSON, err := json.MarshalIndent(serverConfig, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serverPath, serverJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	startSingBoxServer(t, binary, serverPath, ports)

	host := "127.0.0.1"
	cases := []echoTestCase{
		{"vless-vision", fmt.Sprintf("vless://%s@%s:%d?security=tls&flow=xtls-rprx-vision&sni=localhost&allowInsecure=1&fp=chrome", testUUID, host, ports["vless-vision"])},
		{"vless-grpc", fmt.Sprintf("vless://%s@%s:%d?security=tls&type=grpc&serviceName=grpcSvc&sni=localhost&allowInsecure=1", testUUID, host, ports["vless-grpc"])},
		{"vless-httpupgrade", fmt.Sprintf("vless://%s@%s:%d?security=tls&type=httpupgrade&host=localhost&path=%%2Fup&sni=localhost&allowInsecure=1", testUUID, host, ports["vless-httpupgrade"])},
		{"vless-ws-ed", fmt.Sprintf("vless://%s@%s:%d?security=tls&type=ws&path=%%2Fwsed%%3Fed%%3D2048&sni=localhost&allowInsecure=1", testUUID, host, ports["vless-ws-ed"])},
		{"vmess-tcp", fmt.Sprintf("vmess://%s@%s:%d?encryption=auto", testUUID, host, ports["vmess-tcp"])},
		{"vmess-ws", fmt.Sprintf("vmess://%s@%s:%d?encryption=chacha20-poly1305&type=ws&path=%%2Fvmws", testUUID, host, ports["vmess-ws"])},
		{"trojan-grpc", fmt.Sprintf("trojan://%s@%s:%d?security=tls&type=grpc&serviceName=tgrpcSvc&sni=localhost&allowInsecure=1", testTrojanPass, host, ports["trojan-grpc"])},
		{"ss2022", fmt.Sprintf("ss://2022-blake3-aes-256-gcm:%s@%s:%d", testSS2022Key, host, ports["ss2022"])},
		{"hy2", fmt.Sprintf("hy2://%s@%s:%d?insecure=1&sni=localhost&obfs=salamander&obfs-password=%s", testHy2Password, host, ports["hy2"], testHy2ObfsPW)},
		{"tuic", fmt.Sprintf("tuic://%s:%s@%s:%d?insecure=1&sni=localhost&alpn=h3", testUUID, testTUICPass, host, ports["tuic"])},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			socksAddress, ensureErr := Ensure(testCase.link)
			if ensureErr != nil {
				t.Fatalf("Ensure 失败: %v", ensureErr)
			}
			assertEchoRoundTrip(t, socksAddress, echoAddress, testCase.name)
		})
	}
}

// TestSidecarEnsureReuseAndRestart 验证同链接复用与进程死亡自愈
func TestSidecarEnsureReuseAndRestart(t *testing.T) {
	binary, err := resolveBinary(false)
	if err != nil {
		t.Skipf("未找到 sing-box 二进制,跳过: %v", err)
	}
	t.Cleanup(Shutdown)

	echoAddress := startEchoServer(t)
	port, err := pickFreePort()
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := buildTestServerConfig(t, map[string]int{"vmess-tcp": port}, "", "")
	serverPath := filepath.Join(t.TempDir(), "server.json")
	serverJSON, _ := json.MarshalIndent(serverConfig, "", "  ")
	if err := os.WriteFile(serverPath, serverJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	startSingBoxServer(t, binary, serverPath, map[string]int{"vmess-tcp": port})

	link := fmt.Sprintf("vmess://%s@127.0.0.1:%d?encryption=auto", testUUID, port)
	first, err := Ensure(link)
	if err != nil {
		t.Fatalf("首次 Ensure 失败: %v", err)
	}
	assertEchoRoundTrip(t, first, echoAddress, "首次")

	// 复用:同链接应返回同一地址
	again, err := Ensure(link)
	if err != nil {
		t.Fatalf("二次 Ensure 失败: %v", err)
	}
	if again != first {
		t.Fatalf("同链接应复用同一转换层: %q vs %q", first, again)
	}

	// 杀掉转换层进程,Ensure 应自动重建并继续可用
	registryMu.Lock()
	process := registry[link]
	registryMu.Unlock()
	if process == nil {
		t.Fatal("注册表中未找到进程")
	}
	if err := process.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill 失败: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !process.alive() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if process.alive() {
		t.Fatal("进程未被杀死")
	}
	revived, err := Ensure(link)
	if err != nil {
		t.Fatalf("进程死亡后 Ensure 失败: %v", err)
	}
	assertEchoRoundTrip(t, revived, echoAddress, "自愈后")
}

// TestSidecarPassesThroughNative 验证 Ensure 对标准/原生链接原样透传
func TestSidecarPassesThroughNative(t *testing.T) {
	for _, raw := range []string{
		"http://proxy.example.com:8080",
		"socks5://127.0.0.1:1080",
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=none",
	} {
		result, err := Ensure(raw)
		if err != nil {
			t.Fatalf("Ensure(%q) 失败: %v", raw, err)
		}
		if result != raw {
			t.Fatalf("Ensure(%q) 应原样透传,得到 %q", raw, result)
		}
	}
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// startEchoServer 启动逐字节回声服务器
func startEchoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}(conn)
		}
	}()
	return listener.Addr().String()
}

// writeSelfSignedCert 生成 localhost 自签名证书(供 TLS/QUIC inbound 使用)
func writeSelfSignedCert(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(t.TempDir(), "cert.pem")
	keyPath := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// buildTestServerConfig 构造多协议 inbound 的 sing-box 服务端配置
func buildTestServerConfig(t *testing.T, ports map[string]int, certPath, keyPath string) map[string]any {
	t.Helper()
	tlsBlock := map[string]any{"enabled": true, "server_name": "localhost",
		"certificate_path": certPath, "key_path": keyPath}
	inbounds := []map[string]any{}
	if ports["vless-vision"] != 0 {
		inbounds = append(inbounds, map[string]any{
			"type": "vless", "tag": "vless-vision", "listen": "127.0.0.1", "listen_port": ports["vless-vision"],
			"users": []map[string]any{{"uuid": testUUID, "flow": "xtls-rprx-vision"}},
			"tls":   tlsBlock,
		})
	}
	if ports["vless-grpc"] != 0 {
		inbounds = append(inbounds, map[string]any{
			"type": "vless", "tag": "vless-grpc", "listen": "127.0.0.1", "listen_port": ports["vless-grpc"],
			"users":     []map[string]any{{"uuid": testUUID}},
			"transport": map[string]any{"type": "grpc", "service_name": "grpcSvc"},
			"tls":       tlsBlock,
		})
	}
	if ports["vless-httpupgrade"] != 0 {
		inbounds = append(inbounds, map[string]any{
			"type": "vless", "tag": "vless-httpupgrade", "listen": "127.0.0.1", "listen_port": ports["vless-httpupgrade"],
			"users":     []map[string]any{{"uuid": testUUID}},
			"transport": map[string]any{"type": "httpupgrade", "path": "/up"},
			"tls":       tlsBlock,
		})
	}
	if ports["vless-ws-ed"] != 0 {
		inbounds = append(inbounds, map[string]any{
			"type": "vless", "tag": "vless-ws-ed", "listen": "127.0.0.1", "listen_port": ports["vless-ws-ed"],
			"users": []map[string]any{{"uuid": testUUID}},
			"transport": map[string]any{"type": "ws", "path": "/wsed",
				"max_early_data": 2048, "early_data_header_name": "Sec-WebSocket-Protocol"},
			"tls": tlsBlock,
		})
	}
	if ports["vmess-tcp"] != 0 {
		inbounds = append(inbounds, map[string]any{
			"type": "vmess", "tag": "vmess-tcp", "listen": "127.0.0.1", "listen_port": ports["vmess-tcp"],
			"users": []map[string]any{{"uuid": testUUID, "alterId": 0}},
		})
	}
	if ports["vmess-ws"] != 0 {
		inbounds = append(inbounds, map[string]any{
			"type": "vmess", "tag": "vmess-ws", "listen": "127.0.0.1", "listen_port": ports["vmess-ws"],
			"users":     []map[string]any{{"uuid": testUUID, "alterId": 0}},
			"transport": map[string]any{"type": "ws", "path": "/vmws"},
		})
	}
	if ports["trojan-grpc"] != 0 {
		inbounds = append(inbounds, map[string]any{
			"type": "trojan", "tag": "trojan-grpc", "listen": "127.0.0.1", "listen_port": ports["trojan-grpc"],
			"users":     []map[string]any{{"password": testTrojanPass}},
			"transport": map[string]any{"type": "grpc", "service_name": "tgrpcSvc"},
			"tls":       tlsBlock,
		})
	}
	if ports["ss2022"] != 0 {
		inbounds = append(inbounds, map[string]any{
			"type": "shadowsocks", "tag": "ss2022", "listen": "127.0.0.1", "listen_port": ports["ss2022"],
			"method": "2022-blake3-aes-256-gcm", "password": testSS2022Key,
		})
	}
	if ports["hy2"] != 0 {
		inbounds = append(inbounds, map[string]any{
			"type": "hysteria2", "tag": "hy2", "listen": "127.0.0.1", "listen_port": ports["hy2"],
			"users": []map[string]any{{"password": testHy2Password}},
			"obfs":  map[string]any{"type": "salamander", "password": testHy2ObfsPW},
			"tls":   tlsBlock,
		})
	}
	if ports["tuic"] != 0 {
		inbounds = append(inbounds, map[string]any{
			"type": "tuic", "tag": "tuic", "listen": "127.0.0.1", "listen_port": ports["tuic"],
			"users":              []map[string]any{{"uuid": testUUID, "password": testTUICPass}},
			"congestion_control": "cubic",
			"tls": map[string]any{"enabled": true, "server_name": "localhost",
				"certificate_path": certPath, "key_path": keyPath, "alpn": []string{"h3"}},
		})
	}
	return map[string]any{
		"log":      map[string]any{"level": "info", "timestamp": true},
		"inbounds": inbounds,
		"outbounds": []map[string]any{
			{"type": "direct", "tag": "direct"},
		},
		"route": map[string]any{"final": "direct"},
	}
}

// startSingBoxServer 先 check 再拉起服务端进程,
// 等待日志出现 "sing-box started"(同时覆盖 TCP 与 QUIC/UDP 系 inbound)
func startSingBoxServer(t *testing.T, binary, configPath string, ports map[string]int) {
	t.Helper()
	if output, err := exec.Command(binary, "check", "-c", configPath).CombinedOutput(); err != nil {
		t.Fatalf("服务端配置校验失败: %v\n%s", err, output)
	}
	cmd := exec.Command(binary, "run", "--disable-color", "-c", configPath)
	applySysProcAttr(cmd)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr 管道失败: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("服务端启动失败: %v", err)
	}
	exited := make(chan struct{})
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-exited
	})
	output := newLogBuffer(64 * 1024)
	ready := make(chan struct{})
	go func() {
		defer close(exited)
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
		for scanner.Scan() {
			line := scanner.Text()
			output.write([]byte(line + "\n"))
			if strings.Contains(line, "sing-box started") {
				close(ready)
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatalf("服务端启动超时(ports=%v): %s", ports, output.tail(2048))
	}
}

// assertEchoRoundTrip 经 socks5 代理访问回声服务器并校验数据一致
func assertEchoRoundTrip(t *testing.T, socksAddress, echoAddress, label string) {
	t.Helper()
	parsed, _, err := parseSocksHostPort(socksAddress)
	if err != nil {
		t.Fatalf("%s: socks 地址无效 %q: %v", label, socksAddress, err)
	}
	dialer, err := proxy.SOCKS5("tcp", parsed, nil, &net.Dialer{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("%s: 创建 socks 客户端失败: %v", label, err)
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		t.Fatalf("%s: socks 拨号器不支持 context", label)
	}
	var lastErr error
	// QUIC 系协议握手可能略慢,重试数秒
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := contextDialer.DialContext(t.Context(), "tcp", echoAddress)
		if dialErr != nil {
			lastErr = dialErr
			time.Sleep(300 * time.Millisecond)
			continue
		}
		message := fmt.Sprintf("ping-%s-%d", label, time.Now().UnixNano())
		if _, writeErr := conn.Write([]byte(message)); writeErr != nil {
			lastErr = writeErr
			_ = conn.Close()
			continue
		}
		buffer := make([]byte, len(message))
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, readErr := io.ReadFull(conn, buffer); readErr != nil {
			lastErr = readErr
			_ = conn.Close()
			continue
		}
		_ = conn.Close()
		if string(buffer) != message {
			t.Fatalf("%s: 回声不一致: %q vs %q", label, string(buffer), message)
		}
		return
	}
	t.Fatalf("%s: 经代理回声失败: %v", label, lastErr)
}

// parseSocksHostPort 从 socks5://host:port 提取 host:port
func parseSocksHostPort(address string) (string, string, error) {
	trimmed := strings.TrimPrefix(address, "socks5://")
	host, port, err := net.SplitHostPort(trimmed)
	if err != nil {
		return "", "", err
	}
	return net.JoinHostPort(host, port), port, nil
}
