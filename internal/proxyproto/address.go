package proxyproto

import (
	"fmt"
	"net"
	"strconv"
)

// encodeAddress 把目标主机编码为代理协议通用的地址段
//
// 返回值: 类型字节(0x01=IPv4, 0x02=域名, 0x03=IPv6) 与地址体
// (域名带 1 字节长度前缀,其余为原始二进制)
func encodeAddress(host string) (byte, []byte, error) {
	if ip := net.ParseIP(host); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			return 0x01, append([]byte(nil), ipv4...), nil
		}
		return 0x03, append([]byte(nil), ip.To16()...), nil
	}
	if host == "" {
		return 0, nil, fmt.Errorf("目标主机为空")
	}
	if len(host) > 255 {
		return 0, nil, fmt.Errorf("目标域名过长: %d 字节", len(host))
	}
	encoded := make([]byte, 0, 1+len(host))
	encoded = append(encoded, byte(len(host)))
	encoded = append(encoded, host...)
	return 0x02, encoded, nil
}

// parsePort 解析并校验端口号
func parsePort(text string) (int, error) {
	port, err := strconv.Atoi(text)
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("目标端口无效: %q", text)
	}
	return port, nil
}
