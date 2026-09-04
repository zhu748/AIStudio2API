package sidecar

import "sync"

// logBuffer 是有上限的字节缓冲,保留末尾内容(用于进程失败时
// 回显 sing-box 的 stderr 尾部,给用户可定位的错误信息)
type logBuffer struct {
	mu     sync.Mutex
	limit  int
	buffer []byte
}

func newLogBuffer(limit int) *logBuffer {
	return &logBuffer{limit: limit}
}

// write 追加内容,超出上限时丢弃头部
func (b *logBuffer) write(chunk []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buffer = append(b.buffer, chunk...)
	if len(b.buffer) > b.limit {
		b.buffer = b.buffer[len(b.buffer)-b.limit:]
	}
}

// tail 返回缓冲末尾最多 n 字节的内容
func (b *logBuffer) tail(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || len(b.buffer) == 0 {
		return ""
	}
	if len(b.buffer) > n {
		return string(b.buffer[len(b.buffer)-n:])
	}
	return string(b.buffer)
}

// reset 清空缓冲
func (b *logBuffer) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buffer = b.buffer[:0]
}
