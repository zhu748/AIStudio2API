package proxyproto

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

// readConnByte 读单字节(测试辅助)
func readConnByte(conn io.Reader) (byte, error) {
	var buffer [1]byte
	if _, err := io.ReadFull(conn, buffer[:]); err != nil {
		return 0, err
	}
	return buffer[0], nil
}

// ---------------------------------------------------------------------------
// 解析层单元测试
// ---------------------------------------------------------------------------

func TestParseStandardProxyPassthrough(t *testing.T) {
	for _, raw := range []string{
		"http://proxy.example.com:8080",
		"https://proxy.example.com:443",
		"socks5://127.0.0.1:1080",
	} {
		outbound, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q) 失败: %v", raw, err)
		}
		if !outbound.IsStandard() {
			t.Fatalf("Parse(%q) 应识别为标准代理", raw)
		}
	}
}

func TestParseRejectsUnsupported(t *testing.T) {
	cases := []string{
		"vmess://eyJ2IjoiMiIsInBzIjoidGVzdCJ9",                                 // vmess
		"vless://uuid@example.com:443?security=reality",                        // REALITY
		"vless://uuid@example.com:443?flow=xtls-rprx-vision",                   // flow
		"vless://uuid@example.com:443?encryption=aes-128-gcm",                  // encryption != none
		"vless://uuid@example.com:443?type=grpc",                               // grpc 传输
		"ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ=@example.com:8388?plugin=obfs-local", // SIP003 plugin
		"hysteria2://password@example.com:443",                                 // QUIC 系
		"ss2022://xxx@example.com:8388",                                        // 未知 scheme
	}
	for _, raw := range cases {
		if err := Validate(raw); err == nil {
			t.Fatalf("Validate(%q) 应报错", raw)
		}
	}
}

func TestParseVLESSLinkTransport(t *testing.T) {
	raw := "vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?encryption=none&security=tls&sni=cdn.example.com&type=ws&host=cdn.example.com&path=%2Fws#node"
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse 失败: %v", err)
	}
	transport, err := parseLinkTransport(parsed, false)
	if err != nil {
		t.Fatalf("parseLinkTransport 失败: %v", err)
	}
	if transport.network != "ws" || !transport.tlsEnabled {
		t.Fatalf("传输参数解析异常: %+v", transport)
	}
	if transport.sni != "cdn.example.com" || transport.hostHeader != "cdn.example.com" {
		t.Fatalf("sni/host 解析异常: %+v", transport)
	}
	if transport.path != "/ws" {
		t.Fatalf("path 解析异常: %q", transport.path)
	}
	if _, err := Parse(raw); err != nil {
		t.Fatalf("完整 Parse 失败: %v", err)
	}
}

func TestParseTrojanDefaultTLS(t *testing.T) {
	parsed, err := url.Parse("trojan://pass@example.com:443")
	if err != nil {
		t.Fatalf("url.Parse 失败: %v", err)
	}
	transport, err := parseLinkTransport(parsed, true)
	if err != nil {
		t.Fatalf("parseLinkTransport 失败: %v", err)
	}
	if !transport.tlsEnabled {
		t.Fatal("trojan 默认应启用 TLS")
	}
	if transport.network != "tcp" {
		t.Fatalf("默认传输应为 tcp,实际 %s", transport.network)
	}
	if _, err := Parse("trojan://pass@example.com:443"); err != nil {
		t.Fatalf("完整 Parse 失败: %v", err)
	}
}

func TestParseShadowsocksLinks(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		method   string
		password string
		host     string
		port     int
	}{
		{
			name:     "SIP002 base64 userinfo",
			raw:      "ss://" + base64.RawStdEncoding.EncodeToString([]byte("aes-256-gcm:passw0rd")) + "@example.com:8388#node",
			method:   "aes-256-gcm",
			password: "passw0rd",
			host:     "example.com",
			port:     8388,
		},
		{
			name:     "SIP002 明文 userinfo",
			raw:      "ss://chacha20-ietf-poly1305:passw0rd@example.com:8388#node",
			method:   "chacha20-ietf-poly1305",
			password: "passw0rd",
			host:     "example.com",
			port:     8388,
		},
		{
			name:     "旧式整体编码",
			raw:      "ss://" + base64.RawStdEncoding.EncodeToString([]byte("aes-128-gcm:passw0rd@example.com:8388")) + "#node",
			method:   "aes-128-gcm",
			password: "passw0rd",
			host:     "example.com",
			port:     8388,
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			parsed, err := url.Parse(item.raw)
			if err != nil {
				t.Fatalf("url.Parse 失败: %v", err)
			}
			method, password, host, port, err := decodeShadowsocksLink(parsed, item.raw)
			if err != nil {
				t.Fatalf("decodeShadowsocksLink 失败: %v", err)
			}
			if method != item.method || password != item.password || host != item.host || port != item.port {
				t.Fatalf("解析结果异常: %q %q %q %d", method, password, host, port)
			}
		})
	}
}

func TestParseUUID(t *testing.T) {
	for _, text := range []string{
		"b831381d-6324-4d53-ad4f-8cda48b30811",
		"B831381D63244D53AD4F8CDA48B30811",
		"b831381d63244d53ad4f8cda48b30811",
	} {
		uuid, err := parseUUID(text)
		if err != nil {
			t.Fatalf("parseUUID(%q) 失败: %v", text, err)
		}
		if hex.EncodeToString(uuid[:]) != strings.ToLower(strings.ReplaceAll(text, "-", "")) {
			t.Fatalf("UUID 解析结果异常: %x", uuid)
		}
	}
	if _, err := parseUUID("not-a-uuid"); err == nil {
		t.Fatal("无效 UUID 应报错")
	}
}

func TestEncodeAddress(t *testing.T) {
	addrType, body, err := encodeAddress("aistudio.google.com")
	if err != nil {
		t.Fatalf("encodeAddress 失败: %v", err)
	}
	if addrType != 0x02 || int(body[0]) != len("aistudio.google.com") || string(body[1:]) != "aistudio.google.com" {
		t.Fatalf("域名编码异常: type=%d body=%q", addrType, body)
	}
	addrType, body, err = encodeAddress("127.0.0.1")
	if err != nil || addrType != 0x01 || len(body) != 4 || body[0] != 127 {
		t.Fatalf("IPv4 编码异常")
	}
	addrType, body, err = encodeAddress("2001:db8::1")
	if err != nil || addrType != 0x03 || len(body) != 16 {
		t.Fatalf("IPv6 编码异常")
	}
}

// ---------------------------------------------------------------------------
// 协议头字节级断言
// ---------------------------------------------------------------------------

func TestBuildVLESSRequest(t *testing.T) {
	uuid, _ := parseUUID("b831381d-6324-4d53-ad4f-8cda48b30811")
	request, err := buildVLESSRequest(uuid, "aistudio.google.com:443")
	if err != nil {
		t.Fatalf("buildVLESSRequest 失败: %v", err)
	}
	if request[0] != 0x00 {
		t.Fatal("version 应为 0")
	}
	if !bytes.Equal(request[1:17], uuid[:]) {
		t.Fatal("UUID 不匹配")
	}
	if request[17] != 0x00 {
		t.Fatal("addon 长度应为 0")
	}
	if request[18] != 0x01 {
		t.Fatal("command 应为 TCP(0x01)")
	}
	if binary.BigEndian.Uint16(request[19:21]) != 443 {
		t.Fatal("端口应为 443")
	}
	if request[21] != 0x02 {
		t.Fatal("地址类型应为域名(0x02)")
	}
	host := "aistudio.google.com"
	if int(request[22]) != len(host) || string(request[23:]) != host {
		t.Fatal("域名编码不匹配")
	}
}

func TestBuildTrojanRequest(t *testing.T) {
	request, err := buildTrojanRequest("passw0rd", "example.com:443")
	if err != nil {
		t.Fatalf("buildTrojanRequest 失败: %v", err)
	}
	digest := sha256.Sum224([]byte("passw0rd"))
	expected := hex.EncodeToString(digest[:])
	if string(request[:56]) != expected {
		t.Fatal("SHA224 密码摘要不匹配")
	}
	if !bytes.Equal(request[56:58], []byte("\r\n")) {
		t.Fatal("CRLF 缺失")
	}
	if request[58] != 0x01 {
		t.Fatal("command 应为 0x01")
	}
	if binary.BigEndian.Uint16(request[59:61]) != 443 {
		t.Fatal("端口应为 443")
	}
	if request[61] != 0x02 {
		t.Fatal("地址类型应为域名")
	}
	if !bytes.Equal(request[len(request)-2:], []byte("\r\n")) {
		t.Fatal("尾部 CRLF 缺失")
	}
}

// ---------------------------------------------------------------------------
// SS AEAD 编解码 round-trip
// ---------------------------------------------------------------------------

func TestShadowsocksAEADRoundTrip(t *testing.T) {
	node := &shadowsocksOutbound{
		method:  "aes-256-gcm",
		key:     evpBytesToKey("test-password", 32),
		saltLen: 32,
	}
	var stream bytes.Buffer
	writer, err := newShadowsocksWriter(&stream, node, []byte{0x02, 4, 'a', 'b', 'c', 'd', 0x00, 0x50})
	if err != nil {
		t.Fatalf("newShadowsocksWriter 失败: %v", err)
	}
	if _, err := writer.Write([]byte("hello shadowsocks")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	salt := stream.Bytes()[:node.saltLen]
	aead, err := node.newServerAEAD(salt)
	if err != nil {
		t.Fatalf("newServerAEAD 失败: %v", err)
	}
	reader := &aeadChunkReader{conn: bytes.NewReader(stream.Bytes()[node.saltLen:]), aead: aead}
	payload, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	expected := []byte{0x02, 4, 'a', 'b', 'c', 'd', 0x00, 0x50}
	expected = append(expected, []byte("hello shadowsocks")...)
	if !bytes.Equal(payload, expected) {
		t.Fatalf("round-trip 数据不匹配: %q", payload)
	}
}

func TestEVPBytesToKey(t *testing.T) {
	key := evpBytesToKey("hello", 32)
	if len(key) != 32 {
		t.Fatalf("密钥长度应为 32,实际 %d", len(key))
	}
	first16 := md5.Sum([]byte("hello"))
	second16 := md5.Sum(append(append([]byte(nil), first16[:]...), []byte("hello")...))
	if !bytes.Equal(key[:16], first16[:]) || !bytes.Equal(key[16:32], second16[:]) {
		t.Fatal("EVP_BytesToKey 派生与手工迭代不一致")
	}
}

// ---------------------------------------------------------------------------
// 端到端:假 VLESS / Trojan / WS 服务器 + SOCKS5 桥
// ---------------------------------------------------------------------------

// fakeVLESSConn 在服务端连接上解析 VLESS 头并进入 echo 模式
// fakeVLESSConn 在服务端连接上模拟 xray VLESS 行为:
// 解析请求头 → 发送 target → 写响应头 → 同步 echo 到连接关闭。
// 生命周期由本函数管理(阻塞直至客户端断开)。
func fakeVLESSConn(t *testing.T, conn net.Conn, uuidHex string, targets chan<- string) {
	// version(1) + uuid(16) + addonLen(1) + command(1) = 19
	header := make([]byte, 19)
	if _, err := io.ReadFull(conn, header); err != nil {
		return
	}
	if hex.EncodeToString(header[1:17]) != uuidHex {
		t.Errorf("服务端收到的 UUID 不匹配: %s", hex.EncodeToString(header[1:17]))
		return
	}
	var port [2]byte
	if _, err := io.ReadFull(conn, port[:]); err != nil {
		return
	}
	addrType, err := readConnByte(conn)
	if err != nil {
		return
	}
	var host string
	switch addrType {
	case 0x01:
		raw := make([]byte, 4)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = net.IP(raw).String()
	case 0x02:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return
		}
		raw := make([]byte, length[0])
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = string(raw)
	default:
		raw := make([]byte, 16)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return
		}
		host = net.IP(raw).String()
	}
	target := fmt.Sprintf("%s:%d", host, int(port[0])<<8|int(port[1]))
	// VLESS 响应头(无论 TCP/WS 传输都存在)
	if _, err := conn.Write([]byte{0x00, 0x00}); err != nil {
		return
	}
	targets <- target
	_, _ = io.Copy(conn, conn)
}

func TestVLESSEndToEnd(t *testing.T) {
	const uuidHex = "b831381d63244d53ad4f8cda48b30811"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer listener.Close()
	targets := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		fakeVLESSConn(t, conn, uuidHex, targets)
		_ = conn.Close()
	}()

	raw := "vless://b831381d-6324-4d53-ad4f-8cda48b30811@" + listener.Addr().String() + "?security=none"
	outbound, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	conn, err := outbound.DialContext(context.Background(), "tcp", "aistudio.google.com:443")
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buffer := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buffer); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(buffer) != "ping" {
		t.Fatalf("echo 数据不匹配: %q", buffer)
	}
	select {
	case target := <-targets:
		if target != "aistudio.google.com:443" {
			t.Fatalf("服务端解析的目标不匹配: %s", target)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("服务端未收到目标")
	}
}

func TestTrojanEndToEnd(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// 读到首个 CRLF 之前的 hex 摘要
		var header []byte
		single := make([]byte, 1)
		for {
			if _, err := io.ReadFull(conn, single); err != nil {
				return
			}
			header = append(header, single[0])
			if len(header) >= 2 && header[len(header)-2] == '\r' && header[len(header)-1] == '\n' {
				break
			}
		}
		digest := sha256.Sum224([]byte("passw0rd"))
		if string(header[:56]) != hex.EncodeToString(digest[:]) {
			t.Errorf("Trojan 摘要不匹配: %s", header[:56])
			return
		}
		// cmd(1) + port(2) + addrType(1) = 4B,之后 domainLen(1)+domain+CRLF(2)
		rest := make([]byte, 4)
		if _, err := io.ReadFull(conn, rest); err != nil {
			return
		}
		domainLen, err := readConnByte(conn)
		if err != nil {
			return
		}
		domain := make([]byte, domainLen)
		if _, err := io.ReadFull(conn, domain); err != nil {
			return
		}
		trailing := make([]byte, 2)
		if _, err := io.ReadFull(conn, trailing); err != nil {
			return
		}
		_, _ = io.Copy(conn, conn)
	}()

	raw := "trojan://passw0rd@" + listener.Addr().String() + "?security=none"
	outbound, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	conn, err := outbound.DialContext(context.Background(), "tcp", "example.com:80")
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("tro")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buffer := make([]byte, 3)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buffer); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(buffer) != "tro" {
		t.Fatalf("echo 数据不匹配: %q", buffer)
	}
}

func TestVLESSOverWebSocketEndToEnd(t *testing.T) {
	const uuidHex = "b831381d63244d53ad4f8cda48b30811"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// WS 握手:读到空行
		var handshake []byte
		single := make([]byte, 1)
		for {
			if _, err := io.ReadFull(conn, single); err != nil {
				return
			}
			handshake = append(handshake, single[0])
			if bytes.HasSuffix(handshake, []byte("\r\n\r\n")) {
				break
			}
		}
		if !bytes.Contains(handshake, []byte("GET /ws HTTP/1.1")) {
			t.Errorf("WS path 不匹配: %q", handshake[:min(30, len(handshake))])
			return
		}
		if !bytes.Contains(handshake, []byte("Host: ws.example.com")) {
			t.Errorf("WS Host 不匹配")
			return
		}
		response := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: dummy\r\n\r\n"
		if _, err := conn.Write([]byte(response)); err != nil {
			return
		}
		// 服务端帧收发:读首个 masked binary 帧解析 VLESS 头,回帧(响应头+载荷)
		serverReader := bufio.NewReader(conn)
		serverWS := &wsConn{conn: conn, reader: serverReader}
		frame, err := serverWS.readFrame()
		if err != nil {
			t.Errorf("服务端读帧失败: %v", err)
			return
		}
		payload := frame.payload
		if len(payload) < 20 || hex.EncodeToString(payload[1:17]) != uuidHex {
			t.Errorf("WS 帧 VLESS 头不匹配")
			return
		}
		// 首帧应答:仅 VLESS 响应头(真实服务端在解析完请求头后单独写出)
		if err := serverWS.writeFrame(0x2, []byte{0x00, 0x00}); err != nil {
			return
		}
		// 后续帧 echo
		for {
			frame, err := serverWS.readFrame()
			if err != nil {
				return
			}
			if err := serverWS.writeFrame(0x2, frame.payload); err != nil {
				return
			}
		}
	}()

	raw := "vless://b831381d-6324-4d53-ad4f-8cda48b30811@" + listener.Addr().String() +
		"?security=none&type=ws&host=ws.example.com&path=%2Fws"
	outbound, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	conn, err := outbound.DialContext(context.Background(), "tcp", "aistudio.google.com:443")
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer conn.Close()
	// 首 Write 生成首帧(仅 VLESS 头),echo 帧回 [resp 2B + 空] → Read 应拿到 0 字节?
	// 实现中 VLESS over WS 消费响应头:首帧 echo 回来的是 2B 头 + payload[20:](首帧无 payload,即只有 2B)
	if _, err := conn.Write([]byte("wsdata")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buffer := make([]byte, 6)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buffer); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(buffer) != "wsdata" {
		t.Fatalf("echo 数据不匹配: %q", buffer)
	}
}

func TestBridgeEndToEnd(t *testing.T) {
	const uuidHex = "b831381d63244d53ad4f8cda48b30811"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		fakeVLESSConn(t, conn, uuidHex, make(chan string, 1))
		_ = conn.Close()
	}()

	raw := "vless://b831381d-6324-4d53-ad4f-8cda48b30811@" + listener.Addr().String() + "?security=none"
	outbound, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridgeAddress, err := ServeSOCKS5(ctx, outbound)
	if err != nil {
		t.Fatalf("ServeSOCKS5 失败: %v", err)
	}

	parsedBridge, err := url.Parse(bridgeAddress)
	if err != nil {
		t.Fatalf("桥地址解析失败: %v", err)
	}
	conn, err := net.DialTimeout("tcp", parsedBridge.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("连接桥失败: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	// SOCKS5 握手:支持无鉴权
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("发送握手失败: %v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("读取握手应答失败: %v", err)
	}
	if reply[0] != 0x05 || reply[1] != 0x00 {
		t.Fatalf("握手应答异常: %x", reply)
	}
	// CONNECT aistudio.google.com:443(0x01BB)
	request := []byte{0x05, 0x01, 0x00, 0x03, byte(len("aistudio.google.com"))}
	request = append(request, "aistudio.google.com"...)
	request = append(request, 0x01, 0xBB)
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("发送 CONNECT 失败: %v", err)
	}
	connectReply := make([]byte, 10)
	if _, err := io.ReadFull(conn, connectReply); err != nil {
		t.Fatalf("读取 CONNECT 应答失败: %v", err)
	}
	if connectReply[1] != 0x00 {
		t.Fatalf("CONNECT 应答非成功: %x", connectReply[:2])
	}
	if _, err := conn.Write([]byte("bridged")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	echo := make([]byte, 7)
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("读取 echo 失败: %v", err)
	}
	if string(echo) != "bridged" {
		t.Fatalf("echo 数据不匹配: %q", echo)
	}
}

// TestBridgeContextCancel 验证 ctx 取消后桥关闭
func TestBridgeContextCancel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		fakeVLESSConn(t, conn, "00000000000000000000000000000000", make(chan string, 1))
		_ = conn.Close()
	}()

	raw := "vless://00000000-0000-0000-0000-000000000000@" + listener.Addr().String() + "?security=none"
	outbound, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	bridgeAddress, err := ServeSOCKS5(ctx, outbound)
	if err != nil {
		t.Fatalf("ServeSOCKS5 失败: %v", err)
	}
	parsedBridge, _ := url.Parse(bridgeAddress)
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", parsedBridge.Host, 500*time.Millisecond)
		if dialErr != nil {
			break // 已关闭
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("桥在 ctx 取消后未关闭")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
