package config

import "testing"

func TestValidateProxySchemes(t *testing.T) {
	valid := []string{
		"",
		"http://proxy.example.com:8080",
		"https://proxy.example.com:443",
		"socks5://127.0.0.1:1080",
		// VLESS: TCP 直连 + TLS/WS 变体
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=none",
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=tls&sni=cdn.example.com&type=ws&host=cdn.example.com&path=%2Fws",
		// Trojan: 默认 TLS
		"trojan://passw0rd@example.com:443",
		"trojan://passw0rd@example.com:443?security=none&type=ws&path=%2Ft",
		// SS: SIP002 与旧式
		"ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ=@example.com:8388",
		"ss://aes-256-gcm:passw0rd@example.com:8388",
	}
	for _, value := range valid {
		if err := ValidateProxy(value); err != nil {
			t.Errorf("ValidateProxy(%q) 应通过: %v", value, err)
		}
	}

	invalid := []string{
		"not a url",
		"ftp://proxy.example.com:21",
		"vmess://eyJ2IjoiMiJ9",
		"vless://not-a-uuid@example.com:443",
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?security=reality",
		"vless://b831381d-6324-4d53-ad4f-8cda48b30811@example.com:443?flow=xtls-rprx-vision",
		"ss://unknown-cipher:pass@example.com:8388",
		"http://user:pass@proxy.example.com:8080",
		"http://proxy.example.com:8080/path",
	}
	for _, value := range invalid {
		if err := ValidateProxy(value); err == nil {
			t.Errorf("ValidateProxy(%q) 应报错", value)
		}
	}
}
