package config

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestValidateProxySchemes(t *testing.T) {
	vmessJSON := map[string]any{
		"v": "2", "ps": "测试节点", "add": "example.com", "port": "443",
		"id": "b831381d-6324-4d53-ad4f-8cda48b30811", "aid": "0",
		"scy": "auto", "net": "ws", "host": "cdn.example.com", "path": "/ws",
		"tls": "tls", "sni": "cdn.example.com",
	}
	vmessPayload := base64.StdEncoding.EncodeToString([]byte(mustJSON(vmessJSON)))
	valid := []string{
		"",
		"http://proxy.example.com:8080",
		"https://proxy.example.com:443",
		"socks5://127.0.0.1:1080",
		// VLESS: TCP 直连 + TLS/WS 变体
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=none",
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=tls&sni=cdn.example.com&type=ws&host=cdn.example.com&path=%2Fws",
		// VLESS: REALITY + vision(sing-box 转换层)
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=reality&pbk=uQbpmanuRsX87EjqDfzQTaEx0T_gY7ToLAjy6ZTXoQU&sid=0123abcd&fp=chrome&sni=example.com#reality-node",
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=tls&flow=xtls-rprx-vision&sni=example.com",
		// VLESS: grpc 传输
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=tls&type=grpc&serviceName=grpcSvc&sni=example.com",
		// Trojan: 默认 TLS
		"trojan://passw0rd@example.com:443",
		"trojan://passw0rd@example.com:443?security=none&type=ws&path=%2Ft",
		// SS: SIP002 与旧式
		"ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ=@example.com:8388",
		"ss://aes-256-gcm:passw0rd@example.com:8388",
		// SS2022(密钥为 32 字节 base64)
		"ss://2022-blake3-aes-256-gcm:MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=@example.com:8388",
		// VMess: base64 JSON 与 URI 两种形态
		"vmess://" + vmessPayload,
		"vmess://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?encryption=auto&security=tls&type=ws&path=%2Fws&sni=example.com#uri-node",
		// Hysteria2 / hy2(端口缺省 443)
		"hysteria2://authkey@example.com:443?sni=example.com&insecure=1#hy2",
		"hy2://authkey@example.com",
		// hysteria v1
		"hysteria://example.com:443/?auth=authkey&peer=example.com&insecure=1&upmbps=100&downmbps=100",
		// TUIC v5
		"tuic://b831381d-6324-4d53-ad4f-8cda48b30811:passw0rd@example.com:443?congestion_control=bbr&udp_relay_mode=native&alpn=h3&sni=example.com",
		// AnyTLS
		"anytls://passw0rd@example.com:443?sni=example.com&insecure=1",
	}
	for _, value := range valid {
		if err := ValidateProxy(value); err != nil {
			t.Errorf("ValidateProxy(%q) 应通过: %v", value, err)
		}
	}

	invalid := []string{
		"not a url",
		"ftp://proxy.example.com:21",
		"vless://not-a-uuid@example.com:443",
		// REALITY 缺少 pbk
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=reality",
		// 旧版 flow 已被各内核移除
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?flow=xtls-rprx-direct",
		// kcp / splithttp 传输
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?type=kcp",
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?type=splithttp",
		// VMess: 无效 base64 / 非法 cipher / kcp
		"vmess://xxxxx",
		"vmess://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?encryption=aes-256-cfb",
		"vmess://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?type=kcp",
		// SS: 未知 method / 2022 密钥长度错误
		"ss://unknown-cipher:pass@example.com:8388",
		"ss://2022-blake3-aes-256-gcm:c2hvcnQ=@example.com:8388",
		// TUIC v4(token)
		"tuic://b831381d-6324-4d53-ad4f-8cda48b30811:pass@example.com:443?token=abc",
		// hysteria2 salamander 缺密码
		"hysteria2://authkey@example.com:443?obfs=salamander",
		// 长尾协议
		"ssr://xxx",
		"wireguard://key@example.com:51820",
		"juicity://uuid:pass@example.com:443",
		// 标准代理旧约束
		"http://user:pass@proxy.example.com:8080",
		"http://proxy.example.com:8080/path",
	}
	for _, value := range invalid {
		if err := ValidateProxy(value); err == nil {
			t.Errorf("ValidateProxy(%q) 应报错", value)
		}
	}
}

// mustJSON 序列化 map(测试辅助)
func mustJSON(value map[string]any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
