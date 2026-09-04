// Package proxyproto 解析 VLESS / Trojan / Shadowsocks 分享链接并建立
// 到代理服务器的拨号通道。
//
// 设计目标:让 PROXY 与账号级 proxy 字段除了 http/https/socks5 之外,
// 还能直接填写 vless:// / trojan:// / ss:// 分享链接(机场或自建节点
// 的标准导出格式),无需在宿主机额外运行 sing-box/xray 转换层。
//
// 支持范围(按传输与安全参数):
//
//	协议     传输              安全层
//	vless    tcp / ws          none / tls
//	trojan   tcp / ws          tls(默认) / none
//	ss       tcp               内建 AEAD 加密
//
// 明确不支持并会在解析时报错的参数(报错信息会给出替代建议):
//   - VLESS 的 REALITY(security=reality)、flow(xtls-rprx-vision 等)
//   - VMess(vmess:// 分享链接)——协议陈旧且客户端实现风险高,
//     请把节点换成 VLESS/Trojan/SS,或本机跑 sing-box 转 socks5 后填 socks5://
//   - hysteria2 / tuic 等 QUIC 系协议(同上,可用 sing-box 转换)
//   - SS2022(blake3 系列 method)、SIP003 plugin(obfs/v2ray-plugin)
//   - grpc / httpupgrade / kcp 等其余传输
//
// 实现全部基于标准库(crypto/tls、crypto/aead、crypto/hkdf),
// 不引入任何第三方依赖,拨号实现满足 context 取消语义。
package proxyproto

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 标准 scheme:由调用方原有 http/https/socks5 逻辑处理,本包只透传
var standardSchemes = map[string]struct{}{
	"http": {}, "https": {}, "socks5": {}, "socks4": {},
}

// dialTimeout 是与代理服务器建立 TCP/TLS/WS 连接的默认超时
const dialTimeout = 30 * time.Second

// Outbound 表示一个解析完成的代理出站:既能给出原始 URL(供日志与
// 桥注册表去重),又能直接作为 context dialer 使用。
type Outbound struct {
	raw      string
	scheme   string
	standard bool
	dial     func(ctx context.Context, network, address string) (net.Conn, error)
}

// IsStandard 报告是否为 http/https/socks 系标准代理 URL
// (此时拨号由调用方原有逻辑处理,本包未生成拨号函数)
func (out *Outbound) IsStandard() bool { return out.standard }

// Raw 返回去除首尾空白后的原始代理字符串
func (out *Outbound) Raw() string { return out.raw }

// DialContext 建立经代理服务器到目标地址的连接
func (out *Outbound) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if out == nil || out.dial == nil {
		return nil, fmt.Errorf("代理出站未初始化")
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("代理仅支持 TCP,收到 %s", network)
	}
	return out.dial(ctx, network, address)
}

// Parse 解析代理 URL。http/https/socks 系返回 IsStandard 的透传结果;
// vless/trojan/ss 解析节点参数并构造拨号函数;其余 scheme 报错。
//
// Parse 只做语法与支持范围校验,不发起任何网络请求,可安全用于
// 启动期配置校验(ValidateProxy)。
func Parse(raw string) (*Outbound, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("代理 URL 为空")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("代理 URL 无效: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if _, ok := standardSchemes[scheme]; ok {
		return &Outbound{raw: raw, scheme: scheme, standard: true}, nil
	}
	var dial func(context.Context, string, string) (net.Conn, error)
	switch scheme {
	case "vless":
		dial, err = parseVLESS(raw, parsed)
	case "trojan":
		dial, err = parseTrojan(raw, parsed)
	case "ss":
		dial, err = parseShadowsocks(raw, parsed)
	case "vmess":
		return nil, fmt.Errorf("vmess:// 暂不支持: 请将节点换为 VLESS/Trojan/SS,或本机运行 sing-box 把 vmess 转成 socks5 后填写 socks5://127.0.0.1:端口")
	case "hysteria2", "hysteria", "tuic", "wireguard", "juicity":
		return nil, fmt.Errorf("%s:// 协议暂不支持: 请本机运行 sing-box/xray 转成 socks5 后填写 socks5://127.0.0.1:端口", scheme)
	default:
		return nil, fmt.Errorf("代理协议 %s 不受支持(可用: http/https/socks5/socks4/vless/trojan/ss)", scheme)
	}
	if err != nil {
		return nil, err
	}
	return &Outbound{raw: raw, scheme: scheme, dial: dial}, nil
}

// Validate 校验代理 URL 语法与支持范围(不发起网络请求)
func Validate(raw string) error {
	_, err := Parse(raw)
	return err
}

// linkTransport 描述分享链接里的传输层与安全层参数(三协议共用)
type linkTransport struct {
	host       string // 代理服务器地址
	port       int    // 代理服务器端口
	network    string // "tcp" 或 "ws"
	path       string // WS path
	hostHeader string // WS Host 头(伪装域名)
	tlsEnabled bool
	sni        string
	insecure   bool
}

// parseLinkTransport 从 URL query 提取传输层参数:
//   - type / network: tcp(默认) / ws;其余报错
//   - security: none / tls(默认 trojan;vless 默认 none);reality 报错
//   - sni / peer: TLS ServerName,缺省用服务器地址
//   - host: WS Host 头,缺省用服务器地址
//   - path / serviceName: WS path,缺省 "/"
//   - allowInsecure / insecure: 跳过证书校验(仅测试环境)
func parseLinkTransport(parsed *url.URL, defaultTLS bool) (linkTransport, error) {
	transport := linkTransport{tlsEnabled: defaultTLS}
	if parsed.Hostname() == "" {
		return transport, fmt.Errorf("代理链接缺少服务器地址")
	}
	transport.host = parsed.Hostname()
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port <= 0 || port > 65535 {
		return transport, fmt.Errorf("代理链接端口无效: %q", parsed.Port())
	}
	transport.port = port

	query := parsed.Query()
	network := lowerQuery(query, "type", "network")
	switch network {
	case "", "tcp", "raw", "none":
		transport.network = "tcp"
	case "ws", "websocket":
		transport.network = "ws"
	case "grpc", "httpupgrade", "h2", "http", "kcp", "quic":
		return transport, fmt.Errorf("传输层 %s 暂不支持(仅支持 tcp/ws): 可在节点服务端开启 ws 传输后重试", network)
	default:
		return transport, fmt.Errorf("传输层 %s 无法识别", network)
	}

	security := lowerQuery(query, "security")
	switch security {
	case "":
		// 使用协议默认
	case "tls":
		transport.tlsEnabled = true
	case "none":
		transport.tlsEnabled = false
	case "reality":
		return transport, fmt.Errorf("VLESS REALITY 暂不支持: 请在节点服务端将 security 改为 tls 后重试")
	default:
		return transport, fmt.Errorf("安全层 %s 无法识别(仅支持 none/tls)", security)
	}

	transport.sni = firstQuery(query, "sni", "peer", "host")
	transport.hostHeader = firstQuery(query, "host")
	transport.path = firstQuery(query, "path", "serviceName")
	if transport.path == "" {
		transport.path = "/"
	}
	insecure := lowerQuery(query, "allowInsecure", "insecure", "allow_insecure")
	if insecure == "1" || insecure == "true" {
		transport.insecure = true
	}
	return transport, nil
}

// dialTransport 建立到代理服务器的底层连接(TCP/TLS/WS 由参数决定)
func (t linkTransport) dialTransport(ctx context.Context) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	address := net.JoinHostPort(t.host, strconv.Itoa(t.port))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("连接代理服务器 %s 失败: %w", address, err)
	}
	if t.network == "ws" {
		conn, err = handshakeWebSocket(ctx, conn, t)
		if err != nil {
			return nil, err
		}
	}
	if t.tlsEnabled {
		sni := t.sni
		if sni == "" {
			sni = t.host
		}
		hostHeader := t.hostHeader
		if hostHeader == "" {
			hostHeader = t.host
		}
		conn, err = upgradeTLS(ctx, conn, sni, hostHeader, t.insecure)
		if err != nil {
			return nil, err
		}
	}
	return conn, nil
}

// lowerQuery 返回第一个非空参数的小写值
func lowerQuery(query url.Values, names ...string) string {
	return strings.ToLower(firstQuery(query, names...))
}

// firstQuery 依序返回第一个非空参数值
func firstQuery(query url.Values, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(query.Get(name)); value != "" {
			return value
		}
	}
	return ""
}
