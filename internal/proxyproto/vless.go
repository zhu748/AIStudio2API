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
//   - type: tcp(默认)/ ws
//   - security: none(默认)/ tls
//   - sni / host / path / allowInsecure: 同 linkTransport
//
// 不支持并报错:
//   - security=reality(REALITY)
//   - flow 非空且非 none(xtls-rprx-vision 等需要 XTLS 流切换)
func parseVLESS(raw string, parsed *url.URL) (func(context.Context, string, string) (net.Conn, error), error) {
	if parsed.User == nil || strings.TrimSpace(parsed.User.Username()) == "" {
		return nil, fmt.Errorf("vless 链接缺少 UUID(应形如 vless://uuid@host:port)")
	}
	uuidText := strings.TrimSpace(parsed.User.Username())
	uuidBytes, err := parseUUID(uuidText)
	if err != nil {
		return nil, fmt.Errorf("vless UUID 无效: %w", err)
	}
	query := parsed.Query()
	if encryption := lowerQuery(query, "encryption"); encryption != "" && encryption != "none" {
		return nil, fmt.Errorf("vless encryption 仅支持 none,收到 %q", encryption)
	}
	if flow := lowerQuery(query, "flow"); flow != "" && flow != "none" {
		return nil, fmt.Errorf("vless flow %q 暂不支持(xtls-rprx-vision 系列需 XTLS 流切换): 请在节点服务端去掉 flow 后重试", flow)
	}
	transport, err := parseLinkTransport(parsed, false)
	if err != nil {
		return nil, fmt.Errorf("vless 链接无效: %w", err)
	}
	node := &vlessOutbound{transport: transport, uuid: uuidBytes}
	return node.dial, nil
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
	// 数据流开头,必须消费掉,否则调用方首读会错位
	var responseHeader [2]byte
	if _, err := io.ReadFull(conn, responseHeader[:]); err != nil {
		return nil, fmt.Errorf("读取 VLESS 响应头失败: %w", err)
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
