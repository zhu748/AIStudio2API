package proxyproto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// shadowsocksOutbound 描述一个 Shadowsocks AEAD 节点
type shadowsocksOutbound struct {
	host    string
	port    int
	method  string
	key     []byte
	saltLen int
}

// 受支持的 AEAD method → (密钥长度, salt 长度)
var shadowsocksAEADMethods = map[string]struct{ keyLen, saltLen int }{
	"aes-128-gcm":             {keyLen: 16, saltLen: 16},
	"aes-256-gcm":             {keyLen: 32, saltLen: 32},
	"chacha20-ietf-poly1305":  {keyLen: 32, saltLen: 32},
	"xchacha20-ietf-poly1305": {keyLen: 32, saltLen: 32},
}

// parseShadowsocks 解析 ss:// 分享链接,支持三种形态:
//
//  1. SIP002: ss://BASE64URL(method:password)@host:port/?plugin=...#tag
//  2. 旧式整体编码: ss://BASE64URL(method:password@host:port)#tag
//  3. SIP002 明文: ss://method:password@host:port#tag
//
// 处理分级:
//   - AEAD method 无 plugin → 进程内原生拨号
//   - SS2022(2022-blake3 系)与 SIP003 plugin(obfs/v2ray-plugin)
//     → sing-box 转换层承载
//   - 旧版流式 method(rc4-md5、aes-*-cfb 等)已被各内核移除,直接报错
func parseShadowsocks(raw string, parsed *url.URL) (func(context.Context, string, string) (net.Conn, error), *SidecarNode, error) {
	method, password, host, port, err := decodeShadowsocksLink(parsed, raw)
	if err != nil {
		return nil, nil, err
	}
	method = strings.ToLower(strings.TrimSpace(method))
	query := parsed.Query()
	pluginParam := firstQuery(query, "plugin")
	if isSS2022Method(method) || pluginParam != "" {
		if !isSS2022Method(method) {
			if _, ok := shadowsocksAEADMethods[method]; !ok {
				return nil, nil, fmt.Errorf("SS method %q 不受支持(仅支持 AEAD 与 2022-blake3 系)", method)
			}
		} else if err := validateSS2022Key(method, password); err != nil {
			return nil, nil, err
		}
		plugin, pluginOpts := splitSIP003Plugin(pluginParam)
		node, sidecarErr := buildShadowsocksSidecar(method, password, host, port, plugin, pluginOpts, linkTag(parsed))
		if sidecarErr != nil {
			return nil, nil, fmt.Errorf("ss 链接无效: %w", sidecarErr)
		}
		return nil, node, nil
	}
	spec, ok := shadowsocksAEADMethods[method]
	if !ok {
		return nil, nil, fmt.Errorf("SS method %q 不受支持(仅支持 AEAD: aes-128-gcm/aes-256-gcm/chacha20-ietf-poly1305,或 2022-blake3 系)", method)
	}
	node := &shadowsocksOutbound{
		host: host, port: port, method: method,
		key:     evpBytesToKey(password, spec.keyLen),
		saltLen: spec.saltLen,
	}
	return node.dial, nil, nil
}

// isSS2022Method 判断是否为 SS2022(2022-blake3)系 method
func isSS2022Method(method string) bool {
	return strings.HasPrefix(method, "2022-blake3-")
}

// validateSS2022Key 校验 SS2022 密钥:password 本身是 base64 密钥,
// 长度必须与 method 匹配(16/32 字节)
func validateSS2022Key(method, password string) error {
	required := 0
	switch method {
	case "2022-blake3-aes-128-gcm":
		required = 16
	case "2022-blake3-aes-256-gcm":
		required = 32
	default:
		return fmt.Errorf("SS2022 method %q 不受支持(可用: 2022-blake3-aes-128-gcm/2022-blake3-aes-256-gcm)", method)
	}
	if strings.Contains(password, ":") {
		return fmt.Errorf("SS2022 多用户密钥列表不受支持: password 应为单个 base64 密钥")
	}
	decoded, err := decodeBase64Bytes(password)
	if err != nil || len(decoded) != required {
		return fmt.Errorf("SS2022 密钥应为 %d 字节的 base64 编码", required)
	}
	return nil
}

// decodeShadowsocksLink 从三种链接形态中提取 method/password/host/port。
// 旧式整体编码不能经过 url.Parse 重建(base64 中的 +/ 会被改写),
// 必须从原始字符串直接剥离前缀与 fragment。
func decodeShadowsocksLink(parsed *url.URL, raw string) (method, password, host string, port int, err error) {
	if parsed.User != nil {
		// SIP002: userinfo 为 base64url(method:password) 或明文 method:password
		userInfo := parsed.User.String()
		method, password = splitUserInfo(decodeBase64Loose(userInfo))
		host = parsed.Hostname()
		port, err = parsePort(parsed.Port())
		if err != nil {
			return
		}
		return
	}
	// 旧式整体编码: ss://base64(method:password@host:port)
	body := strings.TrimPrefix(strings.TrimSpace(raw), "ss://")
	if index := strings.Index(body, "#"); index >= 0 {
		body = body[:index]
	}
	if index := strings.Index(body, "?"); index >= 0 {
		body = body[:index]
	}
	decoded := decodeBase64Loose(body)
	parsed2, parseErr := url.Parse("ss://" + decoded)
	if parseErr != nil || parsed2.User == nil {
		return "", "", "", 0, fmt.Errorf("ss 链接无法解析: 既不是 SIP002 也不是整体 base64 编码")
	}
	method, password = splitUserInfo(parsed2.User.String())
	host = parsed2.Hostname()
	port, err = parsePort(parsed2.Port())
	if err != nil {
		return
	}
	return
}

// decodeBase64Loose 尝试 base64(标准/URL 安全,带/不带填充)解码,
// 失败时按明文返回(兼容明文 userinfo)
func decodeBase64Loose(text string) string {
	trimmed := strings.TrimSpace(text)
	if decoded, err := base64.StdEncoding.WithPadding(base64.NoPadding).DecodeString(strings.TrimRight(trimmed, "=")); err == nil {
		return string(decoded)
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(trimmed); err == nil {
		return string(decoded)
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(trimmed); err == nil {
		return string(decoded)
	}
	return text
}

// splitUserInfo 拆分 method:password(取第一个冒号)
func splitUserInfo(userInfo string) (string, string) {
	if index := strings.Index(userInfo, ":"); index >= 0 {
		return userInfo[:index], userInfo[index+1:]
	}
	return userInfo, ""
}

// evpBytesToKey 实现 Shadowsocks 的 EVP_BytesToKey 密钥派生
// (与 OpenSSL 兼容: 迭代 MD5 直到凑够 keyLen 字节)
func evpBytesToKey(password string, keyLen int) []byte {
	seed := []byte(password)
	var derived []byte
	var previous []byte
	for len(derived) < keyLen {
		combined := append(append([]byte(nil), previous...), seed...)
		sum := md5.Sum(combined)
		previous = sum[:]
		derived = append(derived, sum[:]...)
	}
	return derived[:keyLen]
}

// subkey 通过 HKDF-SHA1 从 (key, salt) 派生会话子密钥
func (node *shadowsocksOutbound) subkey(salt []byte, keyLen int) []byte {
	key, err := hkdf.Key(sha1.New, node.key, salt, "ss-subkey", keyLen)
	if err != nil {
		panic(fmt.Sprintf("SS 子密钥派生失败: %v", err))
	}
	return key
}

// dial 建立经 SS 节点到目标地址的连接。
// 双向载荷均按 AEAD 分块加密,连接包装为 shadowsocksConn。
func (node *shadowsocksOutbound) dial(ctx context.Context, _, address string) (net.Conn, error) {
	conn, err := node.dialTransport(ctx)
	if err != nil {
		return nil, err
	}
	target, err := buildShadowsocksTarget(address)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	writer, err := newShadowsocksWriter(conn, node, target)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &shadowsocksConn{Conn: conn, node: node, writer: writer}, nil
}

func (node *shadowsocksOutbound) dialTransport(ctx context.Context) (net.Conn, error) {
	return linkTransport{host: node.host, port: node.port, network: "tcp", path: "/"}.dialTransport(ctx)
}

// buildShadowsocksTarget 构造首块明文载荷: SOCKS5 地址段 + 端口
func buildShadowsocksTarget(address string) ([]byte, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("目标地址无效: %w", err)
	}
	port, err := parsePort(portText)
	if err != nil {
		return nil, err
	}
	addrType, addrBytes, err := encodeAddress(host)
	if err != nil {
		return nil, err
	}
	target := make([]byte, 0, 1+len(addrBytes)+2)
	target = append(target, addrType)
	target = append(target, addrBytes...)
	target = append(target, byte(port>>8), byte(port))
	return target, nil
}

// newShadowsocksWriter 生成客户端方向的加密流(salt + AEAD 分块)
func newShadowsocksWriter(conn io.Writer, node *shadowsocksOutbound, firstPayload []byte) (*aeadChunkWriter, error) {
	salt, aead, err := node.newClientAEAD()
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(salt); err != nil {
		return nil, fmt.Errorf("发送 SS salt 失败: %w", err)
	}
	return &aeadChunkWriter{conn: conn, aead: aead, pending: firstPayload}, nil
}

// newClientAEAD 生成随机 salt 并派生客户端方向子密钥
func (node *shadowsocksOutbound) newClientAEAD() ([]byte, cipher.AEAD, error) {
	salt := randomBytes(node.saltLen)
	aead, err := newAEAD(node.method, node.subkey(salt, node.keySize()))
	if err != nil {
		return nil, nil, err
	}
	return salt, aead, nil
}

// newServerAEAD 从已读取的响应 salt 派生服务端方向子密钥
func (node *shadowsocksOutbound) newServerAEAD(salt []byte) (cipher.AEAD, error) {
	return newAEAD(node.method, node.subkey(salt, node.keySize()))
}

func (node *shadowsocksOutbound) keySize() int {
	return shadowsocksAEADMethods[node.method].keyLen
}

// newAEAD 按方法构造 AEAD
func newAEAD(method string, key []byte) (cipher.AEAD, error) {
	switch method {
	case "aes-128-gcm", "aes-256-gcm":
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("SS AES 密钥无效: %w", err)
		}
		return cipher.NewGCM(block)
	case "chacha20-ietf-poly1305":
		return chacha20poly1305.New(key)
	case "xchacha20-ietf-poly1305":
		return chacha20poly1305.NewX(key)
	default:
		return nil, fmt.Errorf("SS method %s 无法构造 AEAD", method)
	}
}

// aeadNonce 生成 SS AEAD 的计数 nonce:12 字节小端计数器
type aeadNonce struct {
	counter uint64
}

func (n *aeadNonce) next() []byte {
	nonce := make([]byte, 12)
	binary.LittleEndian.PutUint64(nonce, n.counter)
	n.counter++
	return nonce
}

// aeadChunkWriter 把明文流包装成 SS AEAD 分块:
//
//	[length(2B)][length tag(16B)][payload][payload tag(16B)] ...
//
// 首个分块以 firstPayload(目标地址)开头,与后续写入数据自动拼接。
type aeadChunkWriter struct {
	conn    io.Writer
	aead    cipher.AEAD
	nonce   aeadNonce
	pending []byte
}

// 写入数据时暂存,flushChunk 按 0x3FFF 分块加密写出
func (w *aeadChunkWriter) Write(buffer []byte) (int, error) {
	w.pending = append(w.pending, buffer...)
	for len(w.pending) > 0 {
		chunk := w.pending
		if len(chunk) > 0x3FFF {
			chunk = chunk[:0x3FFF]
		} else {
			// 剩余数据不足一个分块:立即写出(标准实现逐块写)
		}
		if err := w.flushChunk(chunk); err != nil {
			return 0, err
		}
		w.pending = w.pending[len(chunk):]
	}
	return len(buffer), nil
}

func (w *aeadChunkWriter) flushChunk(chunk []byte) error {
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(chunk)))
	sealedLength := w.aead.Seal(nil, w.nonce.next(), length[:], nil)
	sealedPayload := w.aead.Seal(nil, w.nonce.next(), chunk, nil)
	if _, err := w.conn.Write(sealedLength); err != nil {
		return err
	}
	if _, err := w.conn.Write(sealedPayload); err != nil {
		return err
	}
	return nil
}

// shadowsocksConn 组合底层连接与双向加解密
type shadowsocksConn struct {
	net.Conn
	node   *shadowsocksOutbound
	writer *aeadChunkWriter
	reader *aeadChunkReader
}

func (c *shadowsocksConn) Write(buffer []byte) (int, error) {
	return c.writer.Write(buffer)
}

func (c *shadowsocksConn) Read(buffer []byte) (int, error) {
	if c.reader == nil {
		// 首次读取:消费响应 salt 并构造服务端方向 AEAD
		salt := make([]byte, c.node.saltLen)
		if _, err := io.ReadFull(c.Conn, salt); err != nil {
			return 0, fmt.Errorf("读取 SS 响应 salt 失败: %w", err)
		}
		aead, err := c.node.newServerAEAD(salt)
		if err != nil {
			return 0, err
		}
		c.reader = &aeadChunkReader{conn: c.Conn, aead: aead}
	}
	return c.reader.Read(buffer)
}

// aeadChunkReader 解密 SS AEAD 分块流
type aeadChunkReader struct {
	conn    io.Reader
	aead    cipher.AEAD
	nonce   aeadNonce
	pending []byte
	done    bool
}

func (r *aeadChunkReader) Read(buffer []byte) (int, error) {
	for {
		if len(r.pending) > 0 {
			count := copy(buffer, r.pending)
			r.pending = r.pending[count:]
			return count, nil
		}
		if r.done {
			return 0, io.EOF
		}
		chunk, err := r.readChunk()
		if err != nil {
			return 0, err
		}
		if len(chunk) == 0 {
			continue
		}
		count := copy(buffer, chunk)
		r.pending = chunk[count:]
		return count, nil
	}
}

func (r *aeadChunkReader) readChunk() ([]byte, error) {
	var sealedLength [2 + 16]byte
	if _, err := io.ReadFull(r.conn, sealedLength[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			r.done = true
			return nil, io.EOF
		}
		r.done = true
		return nil, err
	}
	length, err := r.aead.Open(nil, r.nonce.next(), sealedLength[:], nil)
	if err != nil {
		return nil, fmt.Errorf("SS 解密长度失败: %w", err)
	}
	if len(length) != 2 {
		return nil, fmt.Errorf("SS 分块长度字段异常")
	}
	payloadLen := int(binary.BigEndian.Uint16(length))
	if payloadLen == 0 {
		// 空块按规范是流终止信号
		r.done = true
		return nil, io.EOF
	}
	sealedPayload := make([]byte, payloadLen+16)
	if _, err := io.ReadFull(r.conn, sealedPayload); err != nil {
		r.done = true
		return nil, err
	}
	payload, err := r.aead.Open(nil, r.nonce.next(), sealedPayload, nil)
	if err != nil {
		return nil, fmt.Errorf("SS 解密载荷失败: %w", err)
	}
	return payload, nil
}

// randomBytes 生成密码学随机字节
func randomBytes(n int) []byte {
	buffer := make([]byte, n)
	if _, err := rand.Read(buffer); err != nil {
		panic(fmt.Sprintf("随机数生成失败: %v", err))
	}
	return buffer
}
