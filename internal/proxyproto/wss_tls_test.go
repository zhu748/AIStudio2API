package proxyproto

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

// generateSelfSignedCert 生成测试用自签名证书(ws+TLS 端到端测试)
func generateSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "ws-tls.example.com"},
		DNSNames:     []string{"ws-tls.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("生成证书失败: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestVLESSOverWebSocketTLSEndToEnd 验证 wss(ws+TLS) 拨号顺序:
// 必须先完成 TLS 握手,再在加密通道上做 WebSocket 升级。
// 此前实现先发明文 WS 握手再套 TLS,ws+tls 节点必然失败——
// 该测试用于防止此回归。
func TestVLESSOverWebSocketTLSEndToEnd(t *testing.T) {
	const uuidHex = "b831381d63244d53ad4f8cda48b30811"
	certificate := generateSelfSignedCert(t)
	serverTLSConfig := &tls.Config{Certificates: []tls.Certificate{certificate}}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer listener.Close()

	handshakeSawTLS := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// 服务端以 TLS 接收:若客户端先发明明文 HTTP Upgrade(旧 bug),
		// TLS 握手在这里直接失败,测试失败
		tlsConn := tls.Server(conn, serverTLSConfig)
		if err := tlsConn.Handshake(); err != nil {
			t.Errorf("服务端 TLS 握手失败(客户端可能在发明文): %v", err)
			return
		}
		close(handshakeSawTLS)
		// WS 握手必须在 TLS 通道内完成
		var handshake []byte
		single := make([]byte, 1)
		for {
			if _, err := io.ReadFull(tlsConn, single); err != nil {
				return
			}
			handshake = append(handshake, single[0])
			if bytes.HasSuffix(handshake, []byte("\r\n\r\n")) {
				break
			}
		}
		if !bytes.Contains(handshake, []byte("GET /ws HTTP/1.1")) {
			t.Errorf("WS path 不匹配: %q", handshake[:min(30, len(handshake))])
			return
		}
		if !bytes.Contains(handshake, []byte("Host: ws-tls.example.com")) {
			t.Errorf("WS Host 不匹配")
			return
		}
		response := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: dummy\r\n\r\n"
		if _, err := tlsConn.Write([]byte(response)); err != nil {
			return
		}
		// 与 plain WS 测试相同的 VLESS 帧收发
		serverReader := bufio.NewReader(tlsConn)
		serverWS := &wsConn{conn: tlsConn, reader: serverReader}
		frame, err := serverWS.readFrame()
		if err != nil {
			t.Errorf("服务端读帧失败: %v", err)
			return
		}
		if len(frame.payload) < 20 || hex.EncodeToString(frame.payload[1:17]) != uuidHex {
			t.Errorf("WS 帧 VLESS 头不匹配")
			return
		}
		if err := serverWS.writeFrame(0x2, []byte{0x00, 0x00}); err != nil {
			return
		}
		for {
			frame, err := serverWS.readFrame()
			if err != nil {
				return
			}
			if err := serverWS.writeFrame(0x2, frame.payload); err != nil {
				return
			}
		}
	}()

	raw := "vless://b831381d-6324-4d53-ad4f-8cda48b30811@" + listener.Addr().String() +
		"?security=tls&type=ws&host=ws-tls.example.com&sni=ws-tls.example.com&path=%2Fws&allowInsecure=1"
	outbound, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	conn, err := outbound.DialContext(context.Background(), "tcp", "aistudio.google.com:443")
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer conn.Close()
	select {
	case <-handshakeSawTLS:
	case <-time.After(5 * time.Second):
		t.Fatal("TLS 握手未在 5s 内完成")
	}
	if _, err := conn.Write([]byte("wsdata")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buffer := make([]byte, 6)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buffer); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(buffer) != "wsdata" {
		t.Fatalf("echo 数据不匹配: %q", buffer)
	}
}
