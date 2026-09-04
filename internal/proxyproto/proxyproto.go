// Package proxyproto 解析主流代理协议分享链接并建立到代理服务器的
// 拨号通道。
//
// 设计目标:让 PROXY 与账号级 proxy 字段除了 http/https/socks5 之外,
// 还能直接填写机场或自建节点导出的分享链接,无需在宿主机额外
// 运行 sing-box/xray 转换层。
//
// 处理方式分两级:
//
//  1. 进程内原生拨号(零依赖,基于标准库 crypto/tls/aead/hkdf):
//
//     协议     传输              安全层
//     vless    tcp / ws          none / tls
//     trojan   tcp / ws          tls(默认) / none
//     ss       tcp               内建 AEAD 加密
//
//  2. sing-box 转换层(internal/sidecar 拉起 sing-box 子进程转本地 socks5):
//     以下链接解析为 SidecarNode,由调用方交给 internal/sidecar 承载:
//
//     vmess(全部 cipher/传输)、vless REALITY / xtls-rprx-vision /
//     grpc / httpupgrade / h2 / quic 传输、trojan grpc 系传输、
//     SS2022(2022-blake3)、SIP003 plugin、hysteria2(hy2)、
//     hysteria v1、tuic v5、anytls
//
// 明确不支持并会在解析时报错的项(报错信息会给出替代建议):
//   - ssr / juicity / snell / brook / wireguard 等长尾协议
//   - kcp(mkcp)与 splithttp(xhttp)传输(sing-box 不支持)
//
// 原生拨号实现满足 context 取消语义;Parse 只做语法与支持范围校验,
// 不发起任何网络请求。
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
//
// 当链接属于第 2 级支持(sing-box 转换层)时 dial 为空、sidecar 非空,
// 调用方应通过 NeedsSidecar 判定后交给 internal/sidecar 拉起子进程。
type Outbound struct {
	raw      string
	scheme   string
	standard bool
	dial     func(ctx context.Context, network, address string) (net.Conn, error)
	sidecar  *SidecarNode
}

// IsStandard 报告是否为 http/https/socks 系标准代理 URL
// (此时拨号由调用方原有逻辑处理,本包未生成拨号函数)
func (out *Outbound) IsStandard() bool { return out.standard }

// NeedsSidecar 报告是否需要 sing-box 转换层承载
func (out *Outbound) NeedsSidecar() bool { return out != nil && out.sidecar != nil }

// Sidecar 返回 sing-box 转换层节点描述(NeedsSidecar 为 true 时有效)
func (out *Outbound) Sidecar() *SidecarNode { return out.sidecar }

// Raw 返回去除首尾空白后的原始代理字符串
func (out *Outbound) Raw() string { return out.raw }

// DialContext 建立经代理服务器到目标地址的连接
func (out *Outbound) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if out == nil {
		return nil, fmt.Errorf("代理出站未初始化")
	}
	if out.dial == nil {
		if out.sidecar != nil {
			return nil, fmt.Errorf("%s 节点需要 sing-box 转换层: 请经 internal/sidecar.Ensure 获取本地 socks5 地址后再拨号", out.sidecar.Type)
		}
		return nil, fmt.Errorf("代理出站未初始化")
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("代理仅支持 TCP,收到 %s", network)
	}
	return out.dial(ctx, network, address)
}

// Parse 解析代理 URL。http/https/socks 系返回 IsStandard 的透传结果;
// vless/trojan/ss 依参数决定进程内拨号或 SidecarNode;vmess/hysteria2/
// hysteria/tuic/anytls 解析为 SidecarNode;其余 scheme 报错。
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
	var sidecar *SidecarNode
	switch scheme {
	case "vless":
		dial, sidecar, err = parseVLESS(raw, parsed)
	case "trojan":
		dial, sidecar, err = parseTrojan(raw, parsed)
	case "ss":
		dial, sidecar, err = parseShadowsocks(raw, parsed)
	case "vmess":
		sidecar, err = parseVMess(raw)
	case "hysteria2", "hy2":
		sidecar, err = parseHysteria2(raw, parsed)
	case "hysteria":
		sidecar, err = parseHysteria1(raw, parsed)
	case "tuic":
		sidecar, err = parseTUIC(raw, parsed)
	case "anytls":
		sidecar, err = parseAnyTLS(raw, parsed)
	case "wireguard", "juicity", "ssr", "snell", "brook", "naive", "mieru", "ssh", "tor":
		return nil, fmt.Errorf("%s:// 分享链接不受支持: 可将节点换为 vless/vmess/trojan/ss/hysteria2/tuic/anytls,或自建 WireGuard 后用 socks5:// 指向本机转换层", scheme)
	default:
		return nil, fmt.Errorf("代理协议 %s 不受支持(可用: http/https/socks5/socks4/vless/vmess/trojan/ss/hysteria2/tuic/anytls)", scheme)
	}
	if err != nil {
		return nil, err
	}
	return &Outbound{raw: raw, scheme: scheme, dial: dial, sidecar: sidecar}, nil
}

// Validate 校验代理 URL 语法与支持范围(不发起网络请求)
func Validate(raw string) error {
	_, err := Parse(raw)
	return err
}

// SchemeHint 返回脱敏后的代理地址(scheme://host:port),
// 供日志使用,避免打印链接中的凭证(UUID/密码)。
func SchemeHint(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if index := strings.Index(trimmed, "://"); index > 0 {
		rest := trimmed[index+3:]
		// 剥离 userinfo 与查询参数
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		if question := strings.IndexAny(rest, "?#"); question >= 0 {
			rest = rest[:question]
		}
		return trimmed[:index] + "://" + rest
	}
	return "<proxy>"
}

// linkTransport 描述分享链接里的传输层与安全层参数(三协议共用)
type linkTransport struct {
	host        string // 代理服务器地址
	port        int    // 代理服务器端口
	network     string // tcp / ws / grpc / httpupgrade / h2 / quic
	path        string // WS path / grpc serviceName / h2 path
	hostHeader  string // WS Host 头(伪装域名)
	tlsEnabled  bool
	security    string // "" / none / tls / reality(仅 vless)
	realityKey  string // reality public_key(pbk)
	realitySID  string // reality short_id(sid)
	fingerprint string // utls 指纹(fp)
	alpn        []string
	sni         string
	insecure    bool
}

// normalizeTransport 把分享链接里的传输层名称归一化到标准名
func normalizeTransport(name string) (string, error) {
	switch name {
	case "", "tcp", "raw", "none":
		return "tcp", nil
	case "ws", "websocket":
		return "ws", nil
	case "grpc":
		return "grpc", nil
	case "httpupgrade":
		return "httpupgrade", nil
	case "h2", "http":
		return "h2", nil
	case "quic":
		return "quic", nil
	case "kcp", "mkcp":
		return "", fmt.Errorf("传输层 kcp(mkcp)不受支持: sing-box/xray-kcp 节点请改用 tcp/ws/grpc 传输")
	case "splithttp", "xhttp":
		return "", fmt.Errorf("传输层 splithttp(xhttp)不受支持: 该传输仅 xray 内核支持,请把节点换成 tcp/ws/grpc/httpupgrade 传输")
	default:
		return "", fmt.Errorf("传输层 %s 无法识别", name)
	}
}

// parseLinkTransport 从 URL query 提取传输层参数:
//   - type / network: tcp(默认)/ ws / grpc / httpupgrade / h2 / quic
//     (native 拨号仅支持 tcp/ws,其余由调用方转 SidecarNode)
//   - security: none / tls(默认 trojan;vless 默认 none);reality 记录待 vless 侧处理
//   - sni / peer: TLS ServerName,缺省用服务器地址
//   - host: WS Host 头,缺省用服务器地址
//   - path / serviceName: WS path,缺省 "/"
//   - fp: utls 指纹;alpn: 逗号分隔列表
//   - allowInsecure / insecure: 跳过证书校验(仅测试环境)
func parseLinkTransport(parsed *url.URL, defaultTLS bool) (linkTransport, error) {
	transport := linkTransport{tlsEnabled: defaultTLS, network: "tcp"}
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
	network, err := normalizeTransport(lowerQuery(query, "type", "network"))
	if err != nil {
		return transport, err
	}
	transport.network = network

	security := lowerQuery(query, "security")
	switch security {
	case "":
		// 使用协议默认
	case "tls":
		transport.tlsEnabled = true
	case "none":
		transport.tlsEnabled = false
	case "reality":
		transport.security = "reality"
		transport.tlsEnabled = true
	default:
		return transport, fmt.Errorf("安全层 %s 无法识别(仅支持 none/tls/reality)", security)
	}
	if transport.security == "" {
		if transport.tlsEnabled {
			transport.security = "tls"
		} else {
			transport.security = "none"
		}
	}

	transport.sni = firstQuery(query, "sni", "peer", "host")
	transport.hostHeader = firstQuery(query, "host")
	transport.path = firstQuery(query, "path", "serviceName")
	if transport.path == "" {
		transport.path = "/"
	}
	transport.fingerprint = normalizeFingerprint(firstQuery(query, "fp", "fingerprint"))
	if alpn := firstQuery(query, "alpn"); alpn != "" {
		for _, item := range strings.Split(alpn, ",") {
			if item = strings.TrimSpace(item); item != "" {
				transport.alpn = append(transport.alpn, item)
			}
		}
	}
	insecure := lowerQuery(query, "allowInsecure", "insecure", "allow_insecure")
	if insecure == "1" || insecure == "true" {
		transport.insecure = true
	}
	return transport, nil
}

// utlsFingerprints 是 sing-box utls 接受的指纹列表
var utlsFingerprints = map[string]struct{}{
	"chrome": {}, "firefox": {}, "edge": {}, "safari": {}, "ios": {},
	"android": {}, "chrome_psk": {}, "chrome_padding_psk": {},
	"randomized": {}, "randomizedalpn": {}, "qq": {}, "wechat": {}}

// normalizeFingerprint 归一并校验 utls 指纹名(空返回空)
func normalizeFingerprint(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "random" {
		return "randomized"
	}
	return value
}

func fingerprintAllowed(value string) bool {
	if value == "" {
		return true
	}
	_, ok := utlsFingerprints[value]
	return ok
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
	// 正确的握手顺序: TCP → TLS → WebSocket。
	// 若先做明文 WS 握手再套 TLS,会把明文 HTTP Upgrade 发到 TLS 端口,
	// ws+tls 节点(Cloudflare CDN 场景/trojan 默认)将必然失败。
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
	if t.network == "ws" {
		conn, err = handshakeWebSocket(ctx, conn, t)
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
