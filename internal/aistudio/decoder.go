package aistudio

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"unicode"
)

// ProtocolEvidenceError 保存无法解释的协议位置和原始值
type ProtocolEvidenceError struct {
	Method string
	Path   string
	Detail string
	Raw    json.RawMessage
}

// PromptFeedbackError 表示上游拒绝当前输入且没有返回候选
type PromptFeedbackError struct {
	Reason string
	Raw    json.RawMessage
}

// Error 返回协议证据错误
func (e *ProtocolEvidenceError) Error() string {
	if e.Method == "" {
		return fmt.Sprintf("协议位置 %s: %s", e.Path, e.Detail)
	}
	return fmt.Sprintf("AI Studio %s 协议位置 %s: %s", e.Method, e.Path, e.Detail)
}

// Error 返回上游输入拒绝原因
func (e *PromptFeedbackError) Error() string {
	return fmt.Sprintf("AI Studio 拒绝当前输入: %s", e.Reason)
}

// Unwrap 将输入拒绝映射为无效请求
func (e *PromptFeedbackError) Unwrap() error {
	return ErrInvalidArgument
}

type sparseJSONReader struct {
	source *bufio.Reader
	// pending 仅在罕见的稀疏插入("null,"/"null]")时使用固定缓冲,
	// 避免为普通字节分配堆内存
	pending       [5]byte
	pendingLength int
	inString      bool
	escaped       bool
	previousToken byte
}

func newSparseJSONReader(source io.Reader) io.Reader {
	return &sparseJSONReader{source: bufio.NewReader(source)}
}

func (r *sparseJSONReader) Read(destination []byte) (int, error) {
	written := 0
	for written < len(destination) {
		if r.pendingLength > 0 {
			count := copy(destination[written:], r.pending[:r.pendingLength])
			r.pendingLength -= count
			// 剩余插入字节前移(至多 4 字节)
			copy(r.pending[:], r.pending[count:count+r.pendingLength])
			written += count
			continue
		}
		if written > 0 && r.source.Buffered() == 0 {
			return written, nil
		}
		value, err := r.source.ReadByte()
		if err != nil {
			if written > 0 {
				return written, nil
			}
			return 0, err
		}
		if insertion, ok := r.normalize(value); ok {
			// 罕见路径:稀疏数组插入,复制到固定缓冲,零堆分配
			r.pendingLength = copy(r.pending[:], insertion)
			continue
		}
		// 常见路径:直接写入目标,零分配
		destination[written] = value
		written++
	}
	return written, nil
}

// normalize 处理稀疏数组规则。返回 (插入字节, true) 表示需要插入
// "null,"/"null]" 前缀;(nil, false) 表示字节本身直接透传。
// 注意:透传路径绝不分配——此前实现每个字节都 make([]byte, 1),
// 1MB 流式响应即百万次堆分配,GC 压力完全主导解码开销。
func (r *sparseJSONReader) normalize(value byte) ([]byte, bool) {
	if r.inString {
		if r.escaped {
			r.escaped = false
		} else if value == '\\' {
			r.escaped = true
		} else if value == '"' {
			r.inString = false
		}
		return nil, false
	}
	if value == '"' {
		r.inString = true
		r.previousToken = value
		return nil, false
	}
	if value == ',' && (r.previousToken == '[' || r.previousToken == ',') {
		r.previousToken = value
		return sparseNullComma, true
	}
	if value == ']' && r.previousToken == ',' {
		r.previousToken = value
		return sparseNullBracket, true
	}
	if !unicode.IsSpace(rune(value)) {
		r.previousToken = value
	}
	return nil, false
}

var (
	sparseNullComma   = []byte("null,")
	sparseNullBracket = []byte("null]")
)

func decodeJSONValue(raw []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(newSparseJSONReader(bytes.NewReader(raw)))
	decoder.UseNumber()
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("JSON+protobuf 包含多个根值")
		}
		return nil, err
	}
	return value, nil
}

func decodeGenerateItems(source io.Reader, consume func(json.RawMessage) error) error {
	decoder := json.NewDecoder(newSparseJSONReader(source))
	decoder.UseNumber()
	rootStart, err := decoder.Token()
	if err != nil {
		return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: err.Error()}
	}
	if delimiter, ok := rootStart.(json.Delim); !ok || delimiter != '[' {
		return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: "根值不是数组"}
	}
	if !decoder.More() {
		return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: "根数组缺少 field 1"}
	}
	fieldStart, err := decoder.Token()
	if err != nil {
		return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$[0]", Detail: err.Error()}
	}
	if delimiter, ok := fieldStart.(json.Delim); !ok || delimiter != '[' {
		return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$[0]", Detail: "field 1 不是 repeated 数组"}
	}
	index := 0
	for decoder.More() {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return &ProtocolEvidenceError{Method: "GenerateContent", Path: fmt.Sprintf("$[0][%d]", index), Detail: err.Error()}
		}
		// RawMessage.UnmarshalJSON 内部已做拷贝,decoder 每次产出独立分配,
		// 此处无需再复制一遍
		if err := consume(raw); err != nil {
			return err
		}
		index++
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		if err != nil {
			return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$[0]", Detail: err.Error()}
		}
		return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$[0]", Detail: "field 1 没有正常结束"}
	}
	fieldIndex := 1
	for decoder.More() {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return &ProtocolEvidenceError{Method: "GenerateContent", Path: fmt.Sprintf("$[%d]", fieldIndex), Detail: err.Error()}
		}
		fieldIndex++
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		if err != nil {
			return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: err.Error()}
		}
		return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: "根数组没有正常结束"}
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: "响应后存在第二个根值", Raw: extra}
		}
		return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: err.Error()}
	}
	return nil
}

func rawArray(raw json.RawMessage, path string, evidence json.RawMessage) ([]json.RawMessage, error) {
	var values []json.RawMessage
	// 一次性 Unmarshal:流式帧的 RawMessage 均为合法 JSON 值,
	// 无需 NewDecoder+Reader 的双重分配
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, &ProtocolEvidenceError{Path: path, Detail: "期望数组", Raw: cloneEvidence(raw, evidence)}
	}
	return values, nil
}

func rawString(raw json.RawMessage, path string, evidence json.RawMessage) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", &ProtocolEvidenceError{Path: path, Detail: "期望字符串", Raw: cloneEvidence(raw, evidence)}
	}
	return value, nil
}

func rawInt64(raw json.RawMessage, path string, evidence json.RawMessage) (int64, error) {
	// 合法 JSON 整数字面量可直接 ParseInt;字符串/null/浮点等形态均会被拒。
	// 相比 json.NewDecoder+UseNumber 每次调用节省 2 次分配与解码器初始化。
	trimmed := bytes.TrimSpace(raw)
	value, err := strconv.ParseInt(string(trimmed), 10, 64)
	if err != nil {
		return 0, &ProtocolEvidenceError{Path: path, Detail: "期望整数", Raw: cloneEvidence(raw, evidence)}
	}
	return value, nil
}

func rawFloat64(raw json.RawMessage, path string, evidence json.RawMessage) (float64, error) {
	// 合法 JSON 数字字面量可直接 ParseFloat(含溢出→ErrRange 语义一致)
	trimmed := bytes.TrimSpace(raw)
	value, err := strconv.ParseFloat(string(trimmed), 64)
	if err != nil {
		return 0, &ProtocolEvidenceError{Path: path, Detail: "期望数字", Raw: cloneEvidence(raw, evidence)}
	}
	return value, nil
}

func rawBool(raw json.RawMessage, path string, evidence json.RawMessage) (bool, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("1")) {
		return true, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("0")) {
		return false, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, &ProtocolEvidenceError{Path: path, Detail: "期望布尔值", Raw: cloneEvidence(raw, evidence)}
	}
	return value, nil
}

func rawAt(values []json.RawMessage, index int) json.RawMessage {
	if index < 0 || index >= len(values) {
		return nil
	}
	return values[index]
}

func isJSONNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func cloneEvidence(primary json.RawMessage, fallback json.RawMessage) json.RawMessage {
	if len(primary) > 0 {
		return append(json.RawMessage(nil), primary...)
	}
	return append(json.RawMessage(nil), fallback...)
}
