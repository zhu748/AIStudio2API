package proxyproto

import (
	"fmt"
	"strconv"
	"strings"
)

// SidecarNode 描述一个需要 sing-box 转换层承载的代理节点。
//
// Outbound JSON 是 sing-box 出站对象(不含 tag,由 internal/sidecar 注入),
// 字段名与 sing-box v1.14 实测 schema 一一对应。Parse 阶段仅做语法与
// 取值校验,不起进程、不发网络请求。
type SidecarNode struct {
	// Type 是 sing-box 出站类型:
	// vless / vmess / trojan / shadowsocks / hysteria2 / hysteria / tuic / anytls
	Type string
	// Tag 是展示名(链接 #tag 或 v2rayN ps 字段),用于日志
	Tag string
	// Outbound 是 sing-box 出站 JSON 对象
	Outbound map[string]any
}

// buildSidecarTLS 构造 sing-box 的 tls 块。
// 返回 nil 表示该节点不带 TLS(VMess/Trojan 无 tls 且非 reality 时)。
func buildSidecarTLS(transport linkTransport, forceEnabled bool) map[string]any {
	if !transport.tlsEnabled && transport.security != "reality" && !forceEnabled {
		return nil
	}
	tls := map[string]any{"enabled": true}
	serverName := transport.sni
	if serverName == "" {
		serverName = transport.host
	}
	if serverName != "" {
		tls["server_name"] = serverName
	}
	if len(transport.alpn) > 0 {
		tls["alpn"] = transport.alpn
	}
	if transport.insecure {
		tls["insecure"] = true
	}
	if transport.fingerprint != "" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": transport.fingerprint}
	}
	if transport.security == "reality" {
		reality := map[string]any{"enabled": true, "public_key": transport.realityKey}
		if transport.realitySID != "" {
			reality["short_id"] = transport.realitySID
		}
		tls["reality"] = reality
	}
	return tls
}

// buildSidecarTransport 构造 sing-box 的 transport 块。
// 返回 nil 表示裸 TCP(sing-box 省略 transport 字段即裸 TCP)。
func buildSidecarTransport(transport linkTransport) (map[string]any, error) {
	switch transport.network {
	case "tcp":
		return nil, nil
	case "ws":
		result := map[string]any{"type": "ws"}
		path := transport.path
		if path == "" {
			path = "/"
		}
		// v2rayN 习惯把 0-RTT 写成 path="/ws?ed=2048",拆给 max_early_data
		if index := strings.Index(path, "?ed="); index >= 0 {
			if size, err := strconv.Atoi(path[index+4:]); err == nil && size > 0 && size <= 16*1024*1024 {
				path = path[:index]
				result["max_early_data"] = size
				result["early_data_header_name"] = "Sec-WebSocket-Protocol"
			}
		}
		result["path"] = path
		if transport.hostHeader != "" {
			result["headers"] = map[string]any{"Host": transport.hostHeader}
		}
		return result, nil
	case "grpc":
		if transport.path == "" || transport.path == "/" {
			return nil, fmt.Errorf("grpc 传输缺少 serviceName(链接应带 path= 或 serviceName= 参数)")
		}
		return map[string]any{"type": "grpc", "service_name": transport.path}, nil
	case "httpupgrade":
		result := map[string]any{"type": "httpupgrade", "path": transport.path}
		if transport.hostHeader != "" {
			result["host"] = transport.hostHeader
		}
		return result, nil
	case "h2":
		hosts := []string{transport.host}
		if transport.hostHeader != "" {
			hosts = []string{transport.hostHeader}
		}
		return map[string]any{"type": "http", "host": hosts, "path": transport.path}, nil
	case "quic":
		return map[string]any{"type": "quic"}, nil
	default:
		return nil, fmt.Errorf("传输层 %s 无法映射到 sing-box", transport.network)
	}
}

// vlessFlowAllowlist 是 sing-box 支持的 VLESS flow
var vlessFlowAllowlist = map[string]struct{}{
	"": {}, "none": {}, "xtls-rprx-vision": {},
}

// buildVLESSSidecar 把 VLESS 链接参数转换为 sing-box vless 出站。
// REALITY / vision flow / grpc 等进阶组合全部由该路径承载。
func buildVLESSSidecar(uuidText string, transport linkTransport, flow string, tag string) (*SidecarNode, error) {
	if !fingerprintAllowed(transport.fingerprint) {
		return nil, fmt.Errorf("utls 指纹 %q 不受支持(可用: chrome/firefox/edge/safari/ios/android/randomized 等)", transport.fingerprint)
	}
	outbound := map[string]any{
		"type":        "vless",
		"server":      transport.host,
		"server_port": transport.port,
		"uuid":        uuidText,
	}
	switch flow {
	case "", "none":
	case "xtls-rprx-vision":
		// vision 强烈依赖 uTLS 指纹,未指定时默认 chrome
		fp := transport.fingerprint
		if fp == "" {
			fp = "chrome"
		}
		transport.fingerprint = fp
		outbound["flow"] = flow
	default:
		return nil, fmt.Errorf("vless flow %q 不受支持(仅支持空或 xtls-rprx-vision): 旧版 xtls-rprx-direct/origin 已被各内核移除,请升级节点", flow)
	}
	if transport.security == "reality" {
		if transport.realityKey == "" {
			return nil, fmt.Errorf("REALITY 链接缺少 pbk(public_key)参数")
		}
		// REALITY 同样默认 uTLS chrome 指纹
		if transport.fingerprint == "" {
			transport.fingerprint = "chrome"
		}
	}
	transportJSON, err := buildSidecarTransport(transport)
	if err != nil {
		return nil, err
	}
	if transportJSON != nil {
		outbound["transport"] = transportJSON
	}
	tlsJSON := buildSidecarTLS(transport, false)
	if transport.security == "reality" && tlsJSON == nil {
		return nil, fmt.Errorf("REALITY 节点必须启用 TLS")
	}
	if tlsJSON != nil {
		outbound["tls"] = tlsJSON
	}
	return &SidecarNode{Type: "vless", Tag: tag, Outbound: outbound}, nil
}

// buildTrojanSidecar 把 Trojan 链接参数转换为 sing-box trojan 出站
func buildTrojanSidecar(password string, transport linkTransport, tag string) (*SidecarNode, error) {
	if transport.security == "reality" {
		return nil, fmt.Errorf("trojan 协议不支持 REALITY: 请改用 vless 节点")
	}
	if !fingerprintAllowed(transport.fingerprint) {
		return nil, fmt.Errorf("utls 指纹 %q 不受支持(可用: chrome/firefox/edge/safari/ios/android/randomized 等)", transport.fingerprint)
	}
	outbound := map[string]any{
		"type":        "trojan",
		"server":      transport.host,
		"server_port": transport.port,
		"password":    password,
	}
	transportJSON, err := buildSidecarTransport(transport)
	if err != nil {
		return nil, err
	}
	if transportJSON != nil {
		outbound["transport"] = transportJSON
	}
	// trojan-gfw 约定默认 TLS;transport 解析层已把默认 security 记为 tls
	tlsJSON := buildSidecarTLS(transport, false)
	if tlsJSON != nil {
		outbound["tls"] = tlsJSON
	}
	return &SidecarNode{Type: "trojan", Tag: tag, Outbound: outbound}, nil
}

// buildShadowsocksSidecar 把 SS2022 / SIP003 plugin 节点转换为
// sing-box shadowsocks 出站。
// password 原样透传:SS2022 的 password 本身就是 base64 编码的密钥。
func buildShadowsocksSidecar(method, password, host string, port int, plugin, pluginOpts, tag string) (*SidecarNode, error) {
	outbound := map[string]any{
		"type":        "shadowsocks",
		"server":      host,
		"server_port": port,
		"method":      method,
		"password":    password,
	}
	if plugin != "" {
		outbound["plugin"] = plugin
		if pluginOpts != "" {
			outbound["plugin_opts"] = pluginOpts
		}
	}
	return &SidecarNode{Type: "shadowsocks", Tag: tag, Outbound: outbound}, nil
}

// splitSIP003Plugin 拆分 SIP003 plugin 参数:
// "obfs-local;obfs=http;obfs-host=example.com" → ("obfs-local", "obfs=http;obfs-host=example.com")
func splitSIP003Plugin(value string) (plugin, opts string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	parts := strings.SplitN(value, ";", 2)
	plugin = strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		opts = strings.TrimSpace(parts[1])
	}
	return plugin, opts
}
