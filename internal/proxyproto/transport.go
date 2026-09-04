package proxyproto

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	cryptotls "crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// upgradeTLS 在已建立的连接上执行 TLS 握手。
// hostHeader 仅用于 SNI 缺省回退。
func upgradeTLS(ctx context.Context, conn net.Conn, sni string, hostHeader string, insecure bool) (net.Conn, error) {
	if sni == "" {
		sni = hostHeader
	}
	config := &cryptotls.Config{ServerName: sni}
	if insecure {
		config.InsecureSkipVerify = true
	}
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(dialTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	tlsConn := cryptotls.Client(conn, config)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("代理 TLS 握手失败(%s): %w", sni, err)
	}
	if err := tlsConn.SetDeadline(time.Time{}); err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	return tlsConn, nil
}

// handshakeWebSocket 在底层连接上完成 HTTP Upgrade 握手,返回帧读写通道。
func handshakeWebSocket(ctx context.Context, conn net.Conn, t linkTransport) (net.Conn, error) {
	host := t.hostHeader
	if host == "" {
		host = t.host
	}
	keyBytes := make([]byte, 16)
	if _, err := cryptorand.Read(keyBytes); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("生成 WebSocket Key 失败: %w", err)
	}
	path := t.path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	request := fmt.Sprintf("GET %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n"+
		"User-Agent: Mozilla/5.0 (X11; Linux x86_64; rv:152.0) Gecko/20100101 Firefox/152.0\r\n"+
		"\r\n", path, host, base64.StdEncoding.EncodeToString(keyBytes))

	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(dialTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if _, err := conn.Write([]byte(request)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("发送 WebSocket 握手失败: %w", err)
	}
	// bufio.Reader 必须保留给帧读取,避免缓冲区中的首帧数据丢失
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("读取 WebSocket 握手响应失败: %w", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		_ = response.Body.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("WebSocket 握手被拒绝: HTTP %d", response.StatusCode)
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &wsConn{conn: conn, reader: reader}, nil
}

// wsConn 在底层连接上实现 WebSocket 帧收发。
// 上层(代理协议头 + 数据流)整体作为 binary 帧载荷:
//   - 写入:每次 Write 生成一个 FIN+binary 帧(客户端必须 mask)
//   - 读取:循环读帧,跳过/响应控制帧,拼接载荷;超过调用方
//     缓冲区的载荷余量暂存在 pending,由后续 Read 消费
type wsConn struct {
	conn    net.Conn
	reader  *bufio.Reader
	pending []byte
}

func (c *wsConn) Read(buffer []byte) (int, error) {
	if len(c.pending) > 0 {
		count := copy(buffer, c.pending)
		c.pending = c.pending[count:]
		return count, nil
	}
	for {
		header, err := c.readFrame()
		if err != nil {
			return 0, err
		}
		switch header.opcode {
		case 0x8: // close
			_ = c.writeFrame(0x8, nil)
			return 0, fmt.Errorf("WebSocket 连接已关闭")
		case 0x9: // ping → pong
			if err := c.writeFrame(0xA, header.payload); err != nil {
				return 0, err
			}
			continue
		case 0xA: // pong
			continue
		}
		payload := header.payload
		if len(payload) == 0 {
			continue
		}
		count := copy(buffer, payload)
		if count < len(payload) {
			c.pending = payload[count:]
		}
		return count, nil
	}
}

func (c *wsConn) Write(buffer []byte) (int, error) {
	if err := c.writeFrame(0x2, buffer); err != nil {
		return 0, err
	}
	return len(buffer), nil
}

func (c *wsConn) Close() error { return c.conn.Close() }

func (c *wsConn) LocalAddr() net.Addr               { return c.conn.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr              { return c.conn.RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error     { return c.conn.SetDeadline(t) }
func (c *wsConn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

// wsFrame 是一个已解 mask 的完整 WebSocket 帧
type wsFrame struct {
	opcode  byte
	payload []byte
}

// readFrame 读取一个帧并解开 mask(若有)
func (c *wsConn) readFrame() (*wsFrame, error) {
	var fixed [2]byte
	if _, err := io.ReadFull(c.reader, fixed[:]); err != nil {
		return nil, err
	}
	frame := &wsFrame{opcode: fixed[0] & 0x0F}
	length := int64(fixed[1] & 0x7F)
	masked := fixed[1]&0x80 != 0
	switch length {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(c.reader, extended[:]); err != nil {
			return nil, err
		}
		length = int64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(c.reader, extended[:]); err != nil {
			return nil, err
		}
		length = int64(binary.BigEndian.Uint64(extended[:]))
	}
	if length < 0 || length > 1<<26 {
		return nil, fmt.Errorf("WebSocket 帧长度异常: %d", length)
	}
	// WS 帧格式: fixed(2B) → [扩展长度] → [mask(4B,若有)] → payload
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
			return nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return nil, err
	}
	if masked {
		for index := range payload {
			payload[index] ^= mask[index%4]
		}
	}
	frame.payload = payload
	return frame, nil
}

// writeFrame 写出客户端帧(FIN 置位,必须 mask)
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	var header bytes.Buffer
	header.WriteByte(0x80 | opcode) // FIN + opcode
	length := len(payload)
	const maskBit = 0x80
	switch {
	case length < 126:
		header.WriteByte(maskBit | byte(length))
	case length <= 0xFFFF:
		header.WriteByte(maskBit | 126)
		var extended [2]byte
		binary.BigEndian.PutUint16(extended[:], uint16(length))
		header.Write(extended[:])
	default:
		header.WriteByte(maskBit | 127)
		var extended [8]byte
		binary.BigEndian.PutUint64(extended[:], uint64(length))
		header.Write(extended[:])
	}
	var mask [4]byte
	if _, err := cryptorand.Read(mask[:]); err != nil {
		return fmt.Errorf("生成 WebSocket mask 失败: %w", err)
	}
	header.Write(mask[:])
	masked := make([]byte, length)
	for index := 0; index < length; index++ {
		masked[index] = payload[index] ^ mask[index%4]
	}
	if _, err := c.conn.Write(header.Bytes()); err != nil {
		return err
	}
	if length > 0 {
		if _, err := c.conn.Write(masked); err != nil {
			return err
		}
	}
	return nil
}
