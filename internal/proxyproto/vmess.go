package proxyproto

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// parseVMess 解析 vmess:// 分享链接,两种形态均支持:
//
//  1. v2rayN 系整体 base64 JSON:vmess://BASE64({"v":"2","ps":"节点名",
//     "add":"host","port":"443","id":"uuid","aid":"0","scy":"auto",
//     "net":"ws","type":"none","host":"h","path":"/p","tls":"tls",
//     "sni":"s","alpn":"h2,http/1.1","fp":"chrome"})
//  2. sing-box 系 URI 参数:vmess://uuid@host:port?encryption=auto&security=tls
//     &type=ws&host=h&path=/p&sni=s&fp=chrome#tag
//
// VMess 协议头(AEAD 认证 + 加密请求头)没有开放的规范文本,各实现
// 跟随 xray-core 演进,手写客户端互通风险高,因此全部经 sing-box 承载。
func parseVMess(raw string) (*SidecarNode, error) {
	body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "vmess://"))
	if body == "" {
		return nil, fmt.Errorf("vmess 链接为空")
	}
	if strings.Contains(body, "@") {
		if node, err := parseVMessURI(raw, body); err == nil {
			return node, nil
		}
	}
	return parseVMessJSON(raw, body)
}

// parseVMessURI 解析 URI 参数形态
func parseVMessURI(raw, body string) (*SidecarNode, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("vmess 链接无效: 缺少 uuid 或服务器地址")
	}
	uuidText := strings.TrimSpace(parsed.User.Username())
	if _, err := parseUUID(uuidText); err != nil {
		return nil, fmt.Errorf("vmess UUID 无效: %w", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("vmess 链接端口无效: %q", parsed.Port())
	}
	query := parsed.Query()
	transport := linkTransport{host: parsed.Hostname(), port: port, network: "tcp"}
	network, err := normalizeTransport(lowerQuery(query, "type", "network"))
	if err != nil {
		return nil, err
	}
	transport.network = network
	switch security := lowerQuery(query, "security"); security {
	case "":
		transport.tlsEnabled = false
	case "tls":
		transport.tlsEnabled = true
	case "none":
		transport.tlsEnabled = false
	case "reality":
		return nil, fmt.Errorf("vmess 协议不支持 REALITY: 请改用 vless 节点")
	default:
		return nil, fmt.Errorf("vmess 安全层 %s 无法识别(仅支持 none/tls)", security)
	}
	transport.security = securityIfEnabled(transport.tlsEnabled)
	transport.sni = firstQuery(query, "sni", "peer", "host")
	transport.hostHeader = firstQuery(query, "host")
	transport.path = firstQuery(query, "path", "serviceName")
	if transport.path == "" {
		transport.path = "/"
	}
	transport.fingerprint = normalizeFingerprint(firstQuery(query, "fp", "fingerprint"))
	transport.alpn = splitALPN(firstQuery(query, "alpn"))
	if insecure := lowerQuery(query, "allowInsecure", "insecure", "allow_insecure"); insecure == "1" || insecure == "true" {
		transport.insecure = true
	}
	securityCipher := lowerQuery(query, "encryption", "securitycipher")
	cipher := "auto"
	if securityCipher != "" {
		cipher = securityCipher
	}
	if headerType := lowerQuery(query, "headerType", "type_header"); headerType == "http" {
		return nil, fmt.Errorf("vmess tcp headerType=http 伪装不受支持: 请把节点改为 ws/grpc 传输")
	}
	return buildVMessSidecar(uuidText, cipher, 0, transport, linkTag(parsed))
}

// parseVMessJSON 解析 v2rayN base64 JSON 形态
func parseVMessJSON(raw, body string) (*SidecarNode, error) {
	decoded, err := decodeBase64Bytes(body)
	if err != nil {
		return nil, fmt.Errorf("vmess 链接不是有效的 base64: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return nil, fmt.Errorf("vmess 链接 JSON 无效: %w", err)
	}
	get := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := payload[key]; ok && value != nil {
				switch typed := value.(type) {
				case string:
					if strings.TrimSpace(typed) != "" {
						return strings.TrimSpace(typed)
					}
				case float64:
					return strconv.FormatFloat(typed, 'f', -1, 64)
				case bool:
					return strconv.FormatBool(typed)
				}
			}
		}
		return ""
	}
	add := get("add", "address")
	if add == "" {
		return nil, fmt.Errorf("vmess 链接缺少服务器地址(add)")
	}
	portText := get("port")
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("vmess 链接端口无效: %q", portText)
	}
	uuidText := get("id", "uuid")
	if _, err := parseUUID(uuidText); err != nil {
		return nil, fmt.Errorf("vmess UUID 无效: %w", err)
	}
	transport := linkTransport{host: add, port: port, network: "tcp"}
	network, err := normalizeTransport(lowerValue(get("net", "network")))
	if err != nil {
		return nil, err
	}
	transport.network = network
	switch tlsMode := lowerValue(get("tls")); tlsMode {
	case "", "none", "false":
		transport.tlsEnabled = false
	case "tls", "true":
		transport.tlsEnabled = true
	default:
		return nil, fmt.Errorf("vmess 链接 tls 字段无法识别: %q", tlsMode)
	}
	transport.security = securityIfEnabled(transport.tlsEnabled)
	transport.sni = firstNonEmpty(get("sni", "peer"), get("host"))
	transport.hostHeader = get("host")
	transport.path = firstNonEmpty(get("path", "serviceName"), "/")
	transport.fingerprint = normalizeFingerprint(get("fp", "fingerprint"))
	transport.alpn = splitALPN(get("alpn"))
	transport.insecure = false
	cipher := lowerValue(get("scy", "security", "encryption"))
	if cipher == "" {
		cipher = "auto"
	}
	// v2rayN 的 type 字段:tcp 伪装 header(kcp header 类型),http 伪装不受支持
	if headerType := lowerValue(get("type", "headerType")); headerType == "http" && transport.network == "tcp" {
		return nil, fmt.Errorf("vmess tcp type=http 伪装不受支持: 请把节点改为 ws/grpc 传输")
	}
	alterID := 0
	if aid := get("aid", "alterId"); aid != "" {
		if parsed, err := strconv.Atoi(aid); err == nil && parsed >= 0 {
			alterID = parsed
		}
	}
	tag := get("ps", "remarks")
	if tag == "" {
		tag = "vmess-" + add
	}
	return buildVMessSidecar(uuidText, cipher, alterID, transport, tag)
}

// buildVMessSidecar 构造 sing-box vmess 出站
func buildVMessSidecar(uuidText, cipher string, alterID int, transport linkTransport, tag string) (*SidecarNode, error) {
	switch cipher {
	case "auto", "aes-128-gcm", "chacha20-poly1305", "none", "zero":
	case "":
		cipher = "auto"
	default:
		return nil, fmt.Errorf("vmess 加密 %q 不受支持(可用: auto/aes-128-gcm/chacha20-poly1305/none/zero)", cipher)
	}
	if !fingerprintAllowed(transport.fingerprint) {
		return nil, fmt.Errorf("utls 指纹 %q 不受支持(可用: chrome/firefox/edge/safari/ios/android/randomized 等)", transport.fingerprint)
	}
	outbound := map[string]any{
		"type":        "vmess",
		"server":      transport.host,
		"server_port": transport.port,
		"uuid":        uuidText,
		"security":    cipher,
		"alter_id":    alterID,
	}
	transportJSON, err := buildSidecarTransport(transport)
	if err != nil {
		return nil, err
	}
	if transportJSON != nil {
		outbound["transport"] = transportJSON
	}
	if tlsJSON := buildSidecarTLS(transport, false); tlsJSON != nil {
		outbound["tls"] = tlsJSON
	}
	return &SidecarNode{Type: "vmess", Tag: tag, Outbound: outbound}, nil
}

// securityIfEnabled 依据是否启用 TLS 返回 security 值
func securityIfEnabled(tlsEnabled bool) string {
	if tlsEnabled {
		return "tls"
	}
	return "none"
}

// splitALPN 拆分逗号分隔的 alpn 列表
func splitALPN(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

// linkTag 返回链接 #fragment 作为展示名
func linkTag(parsed *url.URL) string {
	tag := strings.TrimSpace(parsed.Fragment)
	if tag == "" {
		return ""
	}
	return tag
}

// lowerValue 返回小写并去空白的值
func lowerValue(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// firstNonEmpty 依序返回第一个非空值
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// decodeBase64Bytes 严格宽松解码 base64(std/urlsafe、带/不带 padding 均可),
// 全部变体失败时返回错误
func decodeBase64Bytes(input string) ([]byte, error) {
	candidate := strings.TrimSpace(input)
	// 依次尝试原始 / 补 padding / urlsafe 字符集
	variants := []struct {
		encoding *base64.Encoding
		text     string
	}{
		{base64.StdEncoding, candidate},
		{base64.RawStdEncoding, strings.TrimRight(candidate, "=")},
		{base64.URLEncoding, candidate},
		{base64.RawURLEncoding, strings.TrimRight(candidate, "=")},
	}
	for _, variant := range variants {
		if variant.text == "" {
			continue
		}
		decoded, err := variant.encoding.DecodeString(variant.text)
		if err == nil {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("base64 解码失败")
}
