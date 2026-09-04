package proxyproto

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
)

// ServeSOCKS5 在 127.0.0.1 随机端口启动一个仅支持 CONNECT 的无鉴权
// SOCKS5 服务,把进入的连接转发到指定出站代理。
//
// 用途:Camoufox 等浏览器引擎只接受 http/socks 系代理,无法直接理解
// vless/trojan/ss 分享链接;把分享链接解析成出站后在进程内架桥,
// 浏览器代理填返回的 socks5://127.0.0.1:<port> 即可让全部流量经
// 该协议节点出去。
//
// 生命周期:ctx 取消时监听器关闭,所有会话的出站拨号随 ctx 失败;
// 已建立的透传连接由调用方在进程退出时自然关闭。
func ServeSOCKS5(ctx context.Context, outbound *Outbound) (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("SOCKS5 桥监听启动失败: %w", err)
	}
	address := fmt.Sprintf("socks5://%s", listener.Addr().String())
	go serveSOCKS5(ctx, listener, outbound)
	return address, nil
}

func serveSOCKS5(ctx context.Context, listener net.Listener, outbound *Outbound) {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go handleSOCKS5Session(ctx, conn, outbound)
	}
}

func handleSOCKS5Session(ctx context.Context, conn net.Conn, outbound *Outbound) {
	defer func() {
		_ = conn.Close()
	}()
	// 阶段一:协商方法(仅支持无鉴权 0x00)
	var greeting [2]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		return
	}
	if greeting[0] != 0x05 || greeting[1] == 0 {
		return
	}
	methods := make([]byte, greeting[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	acceptsNoAuth := false
	for _, method := range methods {
		if method == 0x00 {
			acceptsNoAuth = true
			break
		}
	}
	if !acceptsNoAuth {
		_, _ = conn.Write([]byte{0x05, 0xFF})
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	// 阶段二:CONNECT 请求
	var request [4]byte
	if _, err := io.ReadFull(conn, request[:]); err != nil {
		return
	}
	if request[0] != 0x05 || request[1] != 0x01 { // 仅支持 CONNECT
		_, _ = writeSOCKS5Reply(conn, 0x07)
		return
	}
	target, err := readSOCKS5Address(conn, request[3])
	if err != nil {
		_, _ = writeSOCKS5Reply(conn, 0x01)
		return
	}
	upstream, err := outbound.DialContext(ctx, "tcp", target)
	if err != nil {
		_, _ = writeSOCKS5Reply(conn, 0x04)
		return
	}
	defer func() {
		_ = upstream.Close()
	}()
	if _, err := writeSOCKS5Reply(conn, 0x00); err != nil {
		return
	}
	// 阶段三:双向透传
	var relay sync.WaitGroup
	relay.Add(2)
	go func() {
		defer relay.Done()
		_, _ = io.Copy(upstream, conn)
		_ = upstream.Close()
	}()
	go func() {
		defer relay.Done()
		_, _ = io.Copy(conn, upstream)
		_ = conn.Close()
	}()
	relay.Wait()
}

// readSOCKS5Address 读取 SOCKS5 地址段并拼成 host:port
func readSOCKS5Address(conn net.Conn, addrType byte) (string, error) {
	var host []byte
	switch addrType {
	case 0x01:
		host = make([]byte, 4)
	case 0x03:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return "", err
		}
		host = make([]byte, length[0])
	case 0x04:
		host = make([]byte, 16)
	default:
		return "", fmt.Errorf("SOCKS5 地址类型无效: %d", addrType)
	}
	if _, err := io.ReadFull(conn, host); err != nil {
		return "", err
	}
	var port [2]byte
	if _, err := io.ReadFull(conn, port[:]); err != nil {
		return "", err
	}
	hostText := string(host)
	if addrType != 0x03 {
		hostText = net.IP(host).String()
	}
	return net.JoinHostPort(hostText, fmt.Sprintf("%d", int(port[0])<<8|int(port[1]))), nil
}

// writeSOCKS5Reply 写出成功/失败应答(绑定地址填 0.0.0.0:0)
func writeSOCKS5Reply(conn net.Conn, code byte) (int, error) {
	return conn.Write([]byte{0x05, code, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
}
