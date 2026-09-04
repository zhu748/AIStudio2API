package proxyproto

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
)

// vlessOutbound 描述一个 VLESS 节点
type vlessOutbound struct {
	transport linkTransport
	uuid      [16]byte
}

// parseVLESS 解析 vless://uuid@host:port?...#tag 形式的分享链接
//
// 受支持的参数:
//   - encryption: 必须为 none(VLESS 本体不加密)
//   - type: tcp(默认)/ ws → 进程内原生拨号;
//     grpc / httpupgrade / h2 / quic → sing-box 转换层
//   - security: none(默认)/ tls → 原生;reality → 转换层
//   - flow: 空 → 原生;xtls-rprx-vision → 转换层
//   - sni / host / path / allowInsecure / fp / alpn: 透传两层
//
// 返回值: dial 与 sidecar 二选一(均为 nil 视为参数非法)。
func parseVLESS(raw string, parsed *url.URL) (func(context.Context, string, string) (net.Conn, error), *SidecarNode, error) {
	if parsed.User == nil || strings.TrimSpace(parsed.User.Username()) == "" {
		return nil, nil, fmt.Errorf("vless 链接缺少 UUID(应形如 vless://uuid@host:port)")
	}
	uuidText := strings.TrimSpace(parsed.User.Username())
	uuidBytes, err := parseUUID(uuidText)
	if err != nil {
		return nil, nil, fmt.Errorf("vless UUID 无效: %w", err)
	}
	query := parsed.Query()
	if encryption := lowerQuery(query, "encryption"); encryption != "" && encryption != "none" {
		return nil, nil, fmt.Errorf("vless encryption 仅支持 none,收到 %q", encryption)
	}
	flow := lowerQuery(query, "flow")
	transport, err := parseLinkTransport(parsed, false)
	if err != nil {
		return nil, nil, fmt.Errorf("vless 链接无效: %w", err)
	}
	// REALITY 参数在 parseLinkTransport 层已识别 security,凭证在此提取
	if transport.security == "reality" {
		transport.realityKey = firstQuery(query, "pbk", "publicKey", "public_key")
		transport.realitySID = firstQuery(query, "sid", "shortId", "short_id")
	}
	native := transport.network == "tcp" || transport.network == "ws"
	native = native && transport.security != "reality"
	native = native && (flow == "" || flow == "none")
	// ws 0-RTT(path 携带 ?ed=)仅 sing-box 支持 max_early_data,原生
	// 拨号会把整个 path 当握手路径导致服务端 404,必须转转换层
	native = native && !strings.Contains(transport.path, "?ed=")
	if !native {
		node, sidecarErr := buildVLESSSidecar(uuidText, transport, flow, linkTag(parsed))
		if sidecarErr != nil {
			return nil, nil, fmt.Errorf("vless 链接无效: %w", sidecarErr)
		}
		return nil, node, nil
	}
	vless := &vlessOutbound{transport: transport, uuid: uuidBytes}
	return vless.dial, nil, nil
}

// dial 建立经 VLESS 节点到目标地址的 TCP 连接
func (node *vlessOutbound) dial(ctx context.Context, _, address string) (net.Conn, error) {
	conn, err := node.transport.dialTransport(ctx)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = conn.Close()
		}
	}()
	request, err := buildVLESSRequest(node.uuid, address)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(request); err != nil {
		return nil, fmt.Errorf("发送 VLESS 请求头失败: %w", err)
	}
	// 响应头: version(1B) + addon length(1B),消费后裸流透传。
	// xray/sing-box 服务端无论 TCP 还是 WS 传输都会把响应头写进
	// 数据流开头,必须消费掉,否则调用方首读会错位。
	// 读取置于 deadline+ctx 保护下:失联服务器不再永久挂住拨号 goroutine
	var responseHeader [2]byte
	if err := readProtocolHeader(ctx, conn, func() error {
		if _, err := io.ReadFull(conn, responseHeader[:]); err != nil {
			return fmt.Errorf("读取 VLESS 响应头失败: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if responseHeader[0] != 0x00 {
		return nil, fmt.Errorf("VLESS 响应版本不支持: %d", responseHeader[0])
	}
	// addon 数据(若非 0)直接丢弃,规范允许携带扩展但当前版本不使用
	if addon := int(responseHeader[1]); addon > 0 {
		if _, err := io.CopyN(io.Discard, conn, int64(addon)); err != nil {
			return nil, fmt.Errorf("丢弃 VLESS addon 失败: %w", err)
		}
	}
	success = true
	return conn, nil
}

// buildVLESSRequest 构造 VLESS 请求头(无首包数据)
//
// 结构: version(0x00) + UUID(16B) + addonLen(0x00) + command(0x01=TCP)
//
//   - port(2B BE) + addrType + addr + (数据由调用方写入)
func buildVLESSRequest(uuid [16]byte, address string) ([]byte, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("目标地址无效: %w", err)
	}
	port, err := parsePort(portText)
	if err != nil {
		return nil, err
	}
	request := make([]byte, 0, 1+16+1+1+2+1+len(host)+2)
	request = append(request, 0x00)          // version
	request = append(request, uuid[:]...)    // UUID
	request = append(request, 0x00)          // addon length
	request = append(request, 0x01)          // command: TCP
	request = append(request, byte(port>>8)) // port
	request = append(request, byte(port))
	addrType, addrBytes, err := encodeAddress(host)
	if err != nil {
		return nil, err
	}
	request = append(request, addrType)
	request = append(request, addrBytes...)
	return request, nil
}

// parseUUID 接受 36 位带连字符或 32 位纯十六进制的 UUID 文本
func parseUUID(text string) ([16]byte, error) {
	var uuid [16]byte
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(text)), "-", "")
	if len(normalized) != 32 {
		return uuid, fmt.Errorf("UUID 长度异常: %q", text)
	}
	decoded, err := hex.DecodeString(normalized)
	if err != nil {
		return uuid, fmt.Errorf("UUID 十六进制无效: %w", err)
	}
	copy(uuid[:], decoded)
	return uuid, nil
}
