package proxyproto

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// ---------------------------------------------------------------------------
// SidecarNode 分流与 JSON 生成测试(sing-box 出站 schema 对齐)
// ---------------------------------------------------------------------------

// parseOutbound 解析并断言应走 sing-box 转换层
func parseSidecar(t *testing.T, raw string) *SidecarNode {
	t.Helper()
	outbound, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q) 失败: %v", raw, err)
	}
	if outbound.NeedsSidecar() {
		return outbound.Sidecar()
	}
	t.Fatalf("Parse(%q) 应走 sing-box 转换层,实际走了原生/标准路径", raw)
	return nil
}

// parseOutbound 解析并断言应走进程内原生拨号
func parseNative(t *testing.T, raw string) *Outbound {
	t.Helper()
	outbound, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q) 失败: %v", raw, err)
	}
	if !outbound.NeedsSidecar() && !outbound.IsStandard() {
		return outbound
	}
	t.Fatalf("Parse(%q) 应走进程内原生拨号", raw)
	return nil
}

func TestVLESSRealitySidecar(t *testing.T) {
	node := parseSidecar(t, "vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=reality&pbk=uQbpmanuRsX87EjqDfzQTaEx0T_gY7ToLAjy6ZTXoQU&sid=0123abcd&fp=chrome&sni=example.com#reality-node")
	if node.Type != "vless" {
		t.Fatalf("type 应为 vless: %s", node.Type)
	}
	if node.Tag != "reality-node" {
		t.Fatalf("tag 应取链接 fragment: %q", node.Tag)
	}
	if node.Outbound["uuid"] != "b831381d-6324-4d53-ad4f-8cda48b30811" {
		t.Fatalf("uuid 错误: %v", node.Outbound["uuid"])
	}
	if node.Outbound["server"] != "example.com" || node.Outbound["server_port"] != 443 {
		t.Fatalf("server/port 错误: %v %v", node.Outbound["server"], node.Outbound["server_port"])
	}
	tls, ok := node.Outbound["tls"].(map[string]any)
	if !ok || tls["enabled"] != true {
		t.Fatalf("tls 块缺失或未启用: %v", node.Outbound["tls"])
	}
	reality, ok := tls["reality"].(map[string]any)
	if !ok {
		t.Fatalf("reality 块缺失: %v", tls)
	}
	if reality["public_key"] != "uQbpmanuRsX87EjqDfzQTaEx0T_gY7ToLAjy6ZTXoQU" {
		t.Fatalf("pbk 错误: %v", reality["public_key"])
	}
	if reality["short_id"] != "0123abcd" {
		t.Fatalf("sid 错误: %v", reality["short_id"])
	}
	utls, ok := tls["utls"].(map[string]any)
	if !ok || utls["fingerprint"] != "chrome" {
		t.Fatalf("utls 指纹错误: %v", tls["utls"])
	}
}

func TestVLESSRealityWithoutKeyRejected(t *testing.T) {
	if _, err := Parse("vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=reality"); err == nil {
		t.Fatal("REALITY 缺少 pbk 应报错")
	}
}

func TestVLESSLegacyFlowRejected(t *testing.T) {
	if _, err := Parse("vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?flow=xtls-rprx-direct"); err == nil {
		t.Fatal("旧版 flow xtls-rprx-direct 应报错")
	}
}

func TestVLESSVisionSidecar(t *testing.T) {
	node := parseSidecar(t, "vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=tls&flow=xtls-rprx-vision&sni=example.com")
	if node.Outbound["flow"] != "xtls-rprx-vision" {
		t.Fatalf("flow 错误: %v", node.Outbound["flow"])
	}
	tls, _ := node.Outbound["tls"].(map[string]any)
	utls, _ := tls["utls"].(map[string]any)
	if utls == nil || utls["fingerprint"] != "chrome" {
		t.Fatalf("vision 未默认 chrome 指纹: %v", tls)
	}
}

func TestVLESSExoticTransportsSidecar(t *testing.T) {
	cases := map[string]map[string]any{
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=tls&type=grpc&serviceName=grpcSvc&sni=example.com": map[string]any{
			"type": "grpc", "service_name": "grpcSvc"},
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=tls&type=httpupgrade&host=example.com&path=/up": map[string]any{
			"type": "httpupgrade", "path": "/up", "host": "example.com"},
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=tls&type=h2&host=cdn.example.com&path=/h2": map[string]any{
			"type": "http", "path": "/h2"},
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=tls&type=quic": map[string]any{
			"type": "quic"},
	}
	for raw, expect := range cases {
		node := parseSidecar(t, raw)
		transport, ok := node.Outbound["transport"].(map[string]any)
		if !ok {
			t.Fatalf("%q 缺少 transport 块", raw)
		}
		for key, value := range expect {
			if transport[key] != value {
				t.Fatalf("%q transport.%s = %v,期望 %v", raw, key, transport[key], value)
			}
		}
	}
}

func TestVLESSNativeStillWorks(t *testing.T) {
	parseNative(t, "vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=none")
	parseNative(t, "vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=tls&type=ws&path=%2Fws")
}

func TestVLESSWSEarlyData(t *testing.T) {
	node := parseSidecar(t, "vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=tls&type=grpc&serviceName=ed&flow=xtls-rprx-vision&path=/ws%3Fed%3D2048")
	_ = node
	// ws+ed 组合本身走 sidecar(带 flow),校验 ed 拆分逻辑单独覆盖:
	outbound, err := Parse("vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=tls&type=ws&path=/ws%3Fed%3D2048&flow=xtls-rprx-vision")
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	transport, _ := outbound.Sidecar().Outbound["transport"].(map[string]any)
	if transport["type"] != "ws" {
		t.Fatalf("应为 ws 传输: %v", transport)
	}
	if transport["path"] != "/ws" {
		t.Fatalf("path 应剥离 ?ed=: %v", transport["path"])
	}
	if transport["max_early_data"] != 2048 {
		t.Fatalf("max_early_data 错误: %v", transport["max_early_data"])
	}
	if transport["early_data_header_name"] != "Sec-WebSocket-Protocol" {
		t.Fatalf("early_data_header_name 错误: %v", transport["early_data_header_name"])
	}
}

func TestVMessJSONLink(t *testing.T) {
	payload := map[string]any{
		"v": "2", "ps": "日本节点", "add": "vm.example.com", "port": "443",
		"id": "b831381d-6324-4d53-ad4f-8cda48b30811", "aid": "0",
		"scy": "chacha20-poly1305", "net": "ws", "type": "none",
		"host": "cdn.example.com", "path": "/vmws", "tls": "tls",
		"sni": "cdn.example.com", "alpn": "h2,http/1.1", "fp": "chrome",
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	link := "vmess://" + base64.StdEncoding.EncodeToString(encoded)
	node := parseSidecar(t, link)
	if node.Type != "vmess" || node.Tag != "日本节点" {
		t.Fatalf("type/tag 错误: %s %q", node.Type, node.Tag)
	}
	if node.Outbound["security"] != "chacha20-poly1305" {
		t.Fatalf("security 错误: %v", node.Outbound["security"])
	}
	if node.Outbound["alter_id"] != 0 {
		t.Fatalf("alter_id 错误: %v", node.Outbound["alter_id"])
	}
	transport, _ := node.Outbound["transport"].(map[string]any)
	if transport["type"] != "ws" || transport["path"] != "/vmws" {
		t.Fatalf("transport 错误: %v", transport)
	}
	headers, _ := transport["headers"].(map[string]any)
	if headers["Host"] != "cdn.example.com" {
		t.Fatalf("ws Host 错误: %v", headers)
	}
	tls, _ := node.Outbound["tls"].(map[string]any)
	if tls["server_name"] != "cdn.example.com" {
		t.Fatalf("sni 错误: %v", tls["server_name"])
	}
	if alpn, _ := tls["alpn"].([]string); len(alpn) != 2 {
		t.Fatalf("alpn 应有两个元素: %v", tls["alpn"])
	}
	utls, _ := tls["utls"].(map[string]any)
	if utls["fingerprint"] != "chrome" {
		t.Fatalf("fp 错误: %v", tls["utls"])
	}
}

func TestVMessPortNumberJSON(t *testing.T) {
	// port 字段为数字(部分工具导出的形态)
	payload := map[string]any{
		"add": "vm.example.com", "port": 8443,
		"id": "b831381d-6324-4d53-ad4f-8cda48b30811", "net": "tcp",
	}
	encoded, _ := json.Marshal(payload)
	node := parseSidecar(t, "vmess://"+base64.StdEncoding.EncodeToString(encoded))
	if node.Outbound["server_port"] != 8443 {
		t.Fatalf("数字端口解析错误: %v", node.Outbound["server_port"])
	}
	if _, hasTransport := node.Outbound["transport"]; hasTransport {
		t.Fatal("tcp 传输不应有 transport 块")
	}
}

func TestVMessURILink(t *testing.T) {
	node := parseSidecar(t, "vmess://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?encryption=auto&security=tls&type=ws&host=cdn.example.com&path=%2Fws&sni=example.com&allowInsecure=1#uri-node")
	if node.Tag != "uri-node" {
		t.Fatalf("tag 错误: %q", node.Tag)
	}
	if node.Outbound["security"] != "auto" {
		t.Fatalf("security 错误: %v", node.Outbound["security"])
	}
	tls, _ := node.Outbound["tls"].(map[string]any)
	if tls["insecure"] != true {
		t.Fatalf("insecure 错误: %v", tls)
	}
}

func TestVMessInvalidValues(t *testing.T) {
	invalid := []string{
		"vmess://xxxxx",                      // 坏 base64
		"vmess://not-a-uuid@example.com:443", // 坏 UUID
		"vmess://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?encryption=aes-256-cfb", // 坏 cipher
		"vmess://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?type=kcp",               // 坏传输
		"vmess://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=reality",       // vmess 无 reality
	}
	for _, raw := range invalid {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) 应报错", raw)
		}
	}
}

func TestHysteria2Link(t *testing.T) {
	node := parseSidecar(t, "hysteria2://authkey@example.com:443?sni=example.com&insecure=1&obfs=salamander&obfs-password=obfs123&alpn=h3&up=100&down=200&mport=20000-30000,40000-40010&hop_interval=30#hy2-node")
	if node.Type != "hysteria2" {
		t.Fatalf("type 错误: %s", node.Type)
	}
	if node.Outbound["password"] != "authkey" {
		t.Fatalf("password 错误: %v", node.Outbound["password"])
	}
	if node.Outbound["up_mbps"] != 100 || node.Outbound["down_mbps"] != 200 {
		t.Fatalf("带宽错误: %v %v", node.Outbound["up_mbps"], node.Outbound["down_mbps"])
	}
	obfs, _ := node.Outbound["obfs"].(map[string]any)
	if obfs["type"] != "salamander" || obfs["password"] != "obfs123" {
		t.Fatalf("obfs 错误: %v", obfs)
	}
	tls, _ := node.Outbound["tls"].(map[string]any)
	if tls["enabled"] != true || tls["server_name"] != "example.com" || tls["insecure"] != true {
		t.Fatalf("tls 错误: %v", tls)
	}
	ports, _ := node.Outbound["server_ports"].([]string)
	if len(ports) != 2 || ports[0] != "20000:30000" || ports[1] != "40000:40010" {
		t.Fatalf("端口跳跃错误: %v", ports)
	}
	if node.Outbound["hop_interval"] != "30s" {
		t.Fatalf("hop_interval 错误: %v", node.Outbound["hop_interval"])
	}
}

func TestHysteria2DefaultPort(t *testing.T) {
	node := parseSidecar(t, "hy2://authkey@example.com")
	if node.Outbound["server_port"] != 443 {
		t.Fatalf("默认端口应为 443: %v", node.Outbound["server_port"])
	}
}

func TestHysteria1Link(t *testing.T) {
	node := parseSidecar(t, "hysteria://example.com:443/?auth=authkey&peer=example.com&insecure=1&upmbps=100&downmbps=100&obfs=salamander&obfs-password=obfpw")
	if node.Type != "hysteria" {
		t.Fatalf("type 错误: %s", node.Type)
	}
	if node.Outbound["auth_str"] != "authkey" {
		t.Fatalf("auth_str 错误: %v", node.Outbound["auth_str"])
	}
	if node.Outbound["obfs"] != "obfpw" {
		t.Fatalf("hysteria v1 obfs 应为混淆密码字符串: %v", node.Outbound["obfs"])
	}
	tls, _ := node.Outbound["tls"].(map[string]any)
	if alpn, _ := tls["alpn"].([]string); len(alpn) != 1 || alpn[0] != "h3" {
		t.Fatalf("v1 默认 alpn 应为 h3: %v", tls["alpn"])
	}
}

func TestTUICLink(t *testing.T) {
	node := parseSidecar(t, "tuic://b831381d-6324-4d53-ad4f-8cda48b30811:passw0rd@example.com:443?congestion_control=bbr&udp_relay_mode=native&alpn=h3&sni=example.com")
	if node.Type != "tuic" {
		t.Fatalf("type 错误: %s", node.Type)
	}
	if node.Outbound["uuid"] != "b831381d-6324-4d53-ad4f-8cda48b30811" {
		t.Fatalf("uuid 错误: %v", node.Outbound["uuid"])
	}
	if node.Outbound["password"] != "passw0rd" {
		t.Fatalf("password 错误: %v", node.Outbound["password"])
	}
	if node.Outbound["congestion_control"] != "bbr" || node.Outbound["udp_relay_mode"] != "native" {
		t.Fatalf("拥塞控制/UDP 模式错误: %v %v", node.Outbound["congestion_control"], node.Outbound["udp_relay_mode"])
	}
	tls, _ := node.Outbound["tls"].(map[string]any)
	if alpn, _ := tls["alpn"].([]string); len(alpn) != 1 || alpn[0] != "h3" {
		t.Fatalf("alpn 错误: %v", tls["alpn"])
	}
}

func TestTUICV4Rejected(t *testing.T) {
	if _, err := Parse("tuic://b831381d-6324-4d53-ad4f-8cda48b30811:pass@example.com:443?token=abc"); err == nil {
		t.Fatal("TUIC v4(token)应报错")
	}
}

func TestAnyTLSLink(t *testing.T) {
	node := parseSidecar(t, "anytls://passw0rd@example.com:443?sni=example.com&insecure=1")
	if node.Type != "anytls" {
		t.Fatalf("type 错误: %s", node.Type)
	}
	if node.Outbound["password"] != "passw0rd" {
		t.Fatalf("password 错误: %v", node.Outbound["password"])
	}
	tls, _ := node.Outbound["tls"].(map[string]any)
	if tls["enabled"] != true || tls["insecure"] != true {
		t.Fatalf("tls 错误: %v", tls)
	}
}

func TestSS2022Sidecar(t *testing.T) {
	node := parseSidecar(t, "ss://2022-blake3-aes-256-gcm:MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=@example.com:8388#ss22")
	if node.Type != "shadowsocks" {
		t.Fatalf("type 错误: %s", node.Type)
	}
	if node.Outbound["method"] != "2022-blake3-aes-256-gcm" {
		t.Fatalf("method 错误: %v", node.Outbound["method"])
	}
	if node.Outbound["password"] != "MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=" {
		t.Fatalf("password 错误: %v", node.Outbound["password"])
	}
}

func TestSSPluginSidecar(t *testing.T) {
	node := parseSidecar(t, "ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ=@example.com:8388/?plugin=obfs-local%3Bobfs%3Dhttp%3Bobfs-host%3Dexample.com")
	if node.Outbound["plugin"] != "obfs-local" {
		t.Fatalf("plugin 错误: %v", node.Outbound["plugin"])
	}
	if node.Outbound["plugin_opts"] != "obfs=http;obfs-host=example.com" {
		t.Fatalf("plugin_opts 错误: %v", node.Outbound["plugin_opts"])
	}
}

func TestSSNativeStillWorks(t *testing.T) {
	parseNative(t, "ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ=@example.com:8388")
	parseNative(t, "ss://aes-256-gcm:passw0rd@example.com:8388")
}

func TestTrojanExoticTransportSidecar(t *testing.T) {
	node := parseSidecar(t, "trojan://passw0rd@example.com:443?security=tls&type=grpc&serviceName=grpcSvc&sni=example.com")
	if node.Type != "trojan" {
		t.Fatalf("type 错误: %s", node.Type)
	}
	transport, _ := node.Outbound["transport"].(map[string]any)
	if transport["type"] != "grpc" || transport["service_name"] != "grpcSvc" {
		t.Fatalf("transport 错误: %v", transport)
	}
}

func TestUnsupportedTransportsRejected(t *testing.T) {
	for _, raw := range []string{
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?type=kcp",
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?type=splithttp",
		"trojan://passw0rd@example.com:443?type=xhttp",
	} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) 应报错", raw)
		}
	}
}

func TestSchemeHint(t *testing.T) {
	cases := map[string]string{
		"vless://uuid@example.com:443?security=tls#tag":   "vless://example.com:443",
		"hysteria2://secret@node.io:8443?obfs=salamander": "hysteria2://node.io:8443",
		"socks5://127.0.0.1:1080":                         "socks5://127.0.0.1:1080",
		"vmess://eyJhZGQiOiJleGFtcGxlLmNvbSJ9":            "vmess://eyJhZGQiOiJleGFtcGxlLmNvbSJ9",
	}
	for raw, expect := range cases {
		if got := SchemeHint(raw); got != expect {
			t.Errorf("SchemeHint(%q) = %q,期望 %q", raw, got, expect)
		}
	}
}
