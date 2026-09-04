package proxyproto

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// hostPortWithDefault 提取链接 host:port,端口缺省时使用默认值。
// hy2/hysteria/tuic/anytls 系链接端口可省略(约定 443)。
func hostPortWithDefault(parsed *url.URL, defaultPort int) (string, int, error) {
	host := parsed.Hostname()
	if host == "" {
		// hysteria v1 有 host 可能编码在 path 里的旧格式,这里只认标准形态
		return "", 0, fmt.Errorf("代理链接缺少服务器地址")
	}
	portText := parsed.Port()
	if portText == "" {
		return host, defaultPort, nil
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return host, 0, fmt.Errorf("代理链接端口无效: %q", portText)
	}
	return host, port, nil
}

// parseInsecureFlag 解析跳过证书校验的布尔参数
func parseInsecureFlag(query url.Values) bool {
	value := lowerQuery(query, "insecure", "allowInsecure", "allow_insecure")
	return value == "1" || value == "true"
}

// parseALPNQuery 解析 alpn 逗号分隔列表
func parseALPNQuery(query url.Values) []string {
	return splitALPN(firstQuery(query, "alpn"))
}

// normalizeFingerprintQuery 校验并归一 utls 指纹参数
func normalizeFingerprintQuery(query url.Values) (string, error) {
	value := normalizeFingerprint(firstQuery(query, "fp", "fingerprint"))
	if !fingerprintAllowed(value) {
		return "", fmt.Errorf("utls 指纹 %q 不受支持(可用: chrome/firefox/edge/safari/ios/android/randomized 等)", value)
	}
	return value, nil
}

// parseHysteria2 解析 hysteria2:// 与 hy2:// 分享链接:
//
// hysteria2://password@host:port?sni=xx&insecure=1&obfs=salamander
// &obfs-password=yy&alpn=h3&up=100&down=100&mport=20000-30000#tag
//
// 端口缺省 443;TLS 恒开;mport/ports 转为 sing-box 端口跳跃(server_ports)。
func parseHysteria2(raw string, parsed *url.URL) (*SidecarNode, error) {
	host, port, err := hostPortWithDefault(parsed, 443)
	if err != nil {
		return nil, fmt.Errorf("hysteria2 链接无效: %w", err)
	}
	password := ""
	if parsed.User != nil {
		// 密码可能包含 URL 转义字符,Username() 已解码;含冒号时取全部
		password = parsed.User.String()
	}
	query := parsed.Query()
	outbound := map[string]any{
		"type":        "hysteria2",
		"server":      host,
		"server_port": port,
	}
	if password != "" {
		outbound["password"] = password
	}
	if obfs := lowerQuery(query, "obfs"); obfs != "" {
		if obfs != "salamander" {
			return nil, fmt.Errorf("hysteria2 混淆 %q 不受支持(仅支持 salamander)", obfs)
		}
		obfsPassword := firstQuery(query, "obfs-password", "obfsPassword")
		if obfsPassword == "" {
			return nil, fmt.Errorf("hysteria2 salamander 混淆缺少 obfs-password 参数")
		}
		outbound["obfs"] = map[string]any{"type": "salamander", "password": obfsPassword}
	}
	if up := firstQuery(query, "up", "upmbps"); up != "" {
		if mbps, err := strconv.Atoi(up); err == nil && mbps > 0 {
			outbound["up_mbps"] = mbps
		}
	}
	if down := firstQuery(query, "down", "downmbps"); down != "" {
		if mbps, err := strconv.Atoi(down); err == nil && mbps > 0 {
			outbound["down_mbps"] = mbps
		}
	}
	if ports := convertHysteriaPorts(firstQuery(query, "mport", "ports", "server_ports")); len(ports) > 0 {
		outbound["server_ports"] = ports
		if hop := firstQuery(query, "hop_interval", "hop-interval"); hop != "" {
			if seconds, err := strconv.Atoi(hop); err == nil && seconds > 0 {
				outbound["hop_interval"] = strconv.Itoa(seconds) + "s"
			}
		}
	}
	tls := map[string]any{"enabled": true}
	if sni := firstQuery(query, "sni", "peer"); sni != "" {
		tls["server_name"] = sni
	}
	if alpn := parseALPNQuery(query); len(alpn) > 0 {
		tls["alpn"] = alpn
	}
	if parseInsecureFlag(query) {
		tls["insecure"] = true
	}
	outbound["tls"] = tls
	return &SidecarNode{Type: "hysteria2", Tag: linkTag(parsed), Outbound: outbound}, nil
}

// convertHysteriaPorts 把 hysteria 端口跳跃参数转换为 sing-box server_ports:
// "20000-30000,40000-40010" → ["20000:30000","40000:40010"]
// (hysteria 用连字符区间,sing-box 用冒号区间,实测验证)
func convertHysteriaPorts(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var result []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		item = strings.ReplaceAll(item, "-", ":")
		bounds := strings.Split(item, ":")
		valid := len(bounds) == 1 || len(bounds) == 2
		for _, bound := range bounds {
			port, err := strconv.Atoi(bound)
			if err != nil || port <= 0 || port > 65535 {
				valid = false
				break
			}
		}
		if valid {
			result = append(result, item)
		}
	}
	return result
}

// parseHysteria1 解析 hysteria v1 链接:
//
// hysteria://host:port/?auth=STR&peer=sni&insecure=1&upmbps=100
// &downmbps=100&alpn=h3&obfs=salamander&obfs-password=xx
//
// v1 的鉴权在 query 的 auth 参数,而非 userinfo;端口缺省 443。
func parseHysteria1(raw string, parsed *url.URL) (*SidecarNode, error) {
	host, port, err := hostPortWithDefault(parsed, 443)
	if err != nil {
		return nil, fmt.Errorf("hysteria 链接无效: %w", err)
	}
	query := parsed.Query()
	outbound := map[string]any{
		"type":        "hysteria",
		"server":      host,
		"server_port": port,
	}
	if auth := firstQuery(query, "auth", "auth_str"); auth != "" {
		outbound["auth_str"] = auth
	}
	if up := firstQuery(query, "upmbps", "up"); up != "" {
		if mbps, err := strconv.Atoi(up); err == nil && mbps > 0 {
			outbound["up_mbps"] = mbps
		}
	}
	if down := firstQuery(query, "downmbps", "down"); down != "" {
		if mbps, err := strconv.Atoi(down); err == nil && mbps > 0 {
			outbound["down_mbps"] = mbps
		}
	}
	if obfs := lowerQuery(query, "obfs"); obfs != "" {
		if obfs != "salamander" {
			return nil, fmt.Errorf("hysteria 混淆 %q 不受支持(仅支持 salamander)", obfs)
		}
		obfsPassword := firstQuery(query, "obfs-password", "obfsPassword")
		if obfsPassword == "" {
			return nil, fmt.Errorf("hysteria salamander 混淆缺少 obfs-password 参数")
		}
		outbound["obfs"] = obfsPassword
	}
	tls := map[string]any{"enabled": true}
	if peer := firstQuery(query, "peer", "sni"); peer != "" {
		tls["server_name"] = peer
	}
	if alpn := parseALPNQuery(query); len(alpn) > 0 {
		tls["alpn"] = alpn
	} else {
		tls["alpn"] = []string{"h3"}
	}
	if parseInsecureFlag(query) {
		tls["insecure"] = true
	}
	outbound["tls"] = tls
	return &SidecarNode{Type: "hysteria", Tag: linkTag(parsed), Outbound: outbound}, nil
}

// parseTUIC 解析 tuic v5 分享链接:
//
// tuic://uuid:password@host:port?congestion_control=bbr&udp_relay_mode=native
// &alpn=h3&sni=xx&allow_insecure=0#tag
//
// TUIC v4(token 系)已被各内核移除,遇到 token 参数直接报错。
func parseTUIC(raw string, parsed *url.URL) (*SidecarNode, error) {
	host, port, err := hostPortWithDefault(parsed, 443)
	if err != nil {
		return nil, fmt.Errorf("tuic 链接无效: %w", err)
	}
	query := parsed.Query()
	if token := firstQuery(query, "token"); token != "" {
		return nil, fmt.Errorf("TUIC v4(token 鉴权)不受支持: 请把服务端升级到 v5 后使用 tuic://uuid:password@host 形式")
	}
	if parsed.User == nil || strings.TrimSpace(parsed.User.Username()) == "" {
		return nil, fmt.Errorf("tuic 链接缺少 uuid(应形如 tuic://uuid:password@host:port)")
	}
	uuidText := strings.TrimSpace(parsed.User.Username())
	if _, err := parseUUID(uuidText); err != nil {
		return nil, fmt.Errorf("tuic UUID 无效: %w", err)
	}
	password, _ := parsed.User.Password()
	outbound := map[string]any{
		"type":        "tuic",
		"server":      host,
		"server_port": port,
		"uuid":        uuidText,
		"password":    password,
	}
	switch cc := lowerQuery(query, "congestion_control", "congestion-control", "congestionControl"); cc {
	case "", "bbr", "cubic", "new_reno":
		if cc != "" {
			outbound["congestion_control"] = cc
		}
	default:
		return nil, fmt.Errorf("tuic 拥塞控制 %q 无法识别(可用: bbr/cubic/new_reno)", cc)
	}
	switch mode := lowerQuery(query, "udp_relay_mode", "udp-relay-mode"); mode {
	case "", "native", "quic":
		if mode != "" {
			outbound["udp_relay_mode"] = mode
		}
	default:
		return nil, fmt.Errorf("tuic udp_relay_mode %q 无法识别(可用: native/quic)", mode)
	}
	tls := map[string]any{"enabled": true}
	if sni := firstQuery(query, "sni", "peer"); sni != "" {
		tls["server_name"] = sni
	}
	if alpn := parseALPNQuery(query); len(alpn) > 0 {
		tls["alpn"] = alpn
	} else {
		tls["alpn"] = []string{"h3"}
	}
	if parseInsecureFlag(query) {
		tls["insecure"] = true
	}
	outbound["tls"] = tls
	return &SidecarNode{Type: "tuic", Tag: linkTag(parsed), Outbound: outbound}, nil
}

// parseAnyTLS 解析 anytls://password@host:port?sni=xx&insecure=1&alpn=h2#tag
//
// AnyTLS(2024 年新协议)恒走 TLS,端口缺省 443。
func parseAnyTLS(raw string, parsed *url.URL) (*SidecarNode, error) {
	host, port, err := hostPortWithDefault(parsed, 443)
	if err != nil {
		return nil, fmt.Errorf("anytls 链接无效: %w", err)
	}
	if parsed.User == nil || parsed.User.String() == "" {
		return nil, fmt.Errorf("anytls 链接缺少密码(应形如 anytls://password@host:port)")
	}
	password := parsed.User.String()
	query := parsed.Query()
	outbound := map[string]any{
		"type":        "anytls",
		"server":      host,
		"server_port": port,
		"password":    password,
	}
	tls := map[string]any{"enabled": true}
	if sni := firstQuery(query, "sni", "peer"); sni != "" {
		tls["server_name"] = sni
	}
	if alpn := parseALPNQuery(query); len(alpn) > 0 {
		tls["alpn"] = alpn
	}
	if parseInsecureFlag(query) {
		tls["insecure"] = true
	}
	outbound["tls"] = tls
	return &SidecarNode{Type: "anytls", Tag: linkTag(parsed), Outbound: outbound}, nil
}
