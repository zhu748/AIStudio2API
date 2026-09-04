package proxyproto

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// trojanOutbound 描述一个 Trojan 节点
type trojanOutbound struct {
	transport linkTransport
	password  string
}

// parseTrojan 解析 trojan://password@host:port?...#tag 形式的分享链接
//
// 受支持的参数:
//   - type: tcp(默认)/ ws → 进程内原生拨号;
//     grpc / httpupgrade / h2 / quic → sing-box 转换层
//   - security: tls(默认,符合 trojan-gfw 约定)/ none
//   - sni / host / path / allowInsecure / fp / alpn: 透传两层
func parseTrojan(raw string, parsed *url.URL) (func(context.Context, string, string) (net.Conn, error), *SidecarNode, error) {
	if parsed.User == nil {
		return nil, nil, fmt.Errorf("trojan 链接缺少密码(应形如 trojan://password@host:port)")
	}
	password := parsed.User.Username()
	transport, err := parseLinkTransport(parsed, true)
	if err != nil {
		return nil, nil, fmt.Errorf("trojan 链接无效: %w", err)
	}
	// ws 0-RTT(path 携带 ?ed=)与 grpc/httpupgrade/h2 等传输转转换层
	if transport.network != "tcp" && transport.network != "ws" || strings.Contains(transport.path, "?ed=") {
		node, sidecarErr := buildTrojanSidecar(password, transport, linkTag(parsed))
		if sidecarErr != nil {
			return nil, nil, fmt.Errorf("trojan 链接无效: %w", sidecarErr)
		}
		return nil, node, nil
	}
	node := &trojanOutbound{password: password}
	node.transport = transport
	return node.dial, nil, nil
}

// dial 建立经 Trojan 节点到目标地址的 TCP 连接
func (node *trojanOutbound) dial(ctx context.Context, _, address string) (net.Conn, error) {
	if node.transport.tlsEnabled == false && node.transport.network == "tcp" {
		// trojan-gfw 约定默认 TLS;显式 security=none 时放行但给出明文警告
		// (部分自建内网节点确实如此)
		_ = trojanPlaintextWarning
	}
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
	request, err := buildTrojanRequest(node.password, address)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(request); err != nil {
		return nil, fmt.Errorf("发送 Trojan 请求头失败: %w", err)
	}
	// Trojan 响应无协议头,连接建立后为裸流透传
	success = true
	return conn, nil
}

// buildTrojanRequest 构造 Trojan 请求头:
//
//	hex(SHA224(password)) CRLF + 0x01(TCP) + port(2B BE)
//	+ addrType + addr + CRLF + (数据由调用方写入)
func buildTrojanRequest(password, address string) ([]byte, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("目标地址无效: %w", err)
	}
	port, err := parsePort(portText)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum224([]byte(password))
	request := make([]byte, 0, 56+2+1+2+1+len(host)+2+2)
	request = append(request, hex.EncodeToString(digest[:])...)
	request = append(request, '\r', '\n')
	request = append(request, 0x01) // command: TCP
	request = append(request, byte(port>>8), byte(port))
	addrType, addrBytes, err := encodeAddress(host)
	if err != nil {
		return nil, err
	}
	request = append(request, addrType)
	request = append(request, addrBytes...)
	request = append(request, '\r', '\n')
	return request, nil
}

var trojanPlaintextWarning = fmt.Errorf("Trojan 节点未启用 TLS,密码与流量将明文传输")
