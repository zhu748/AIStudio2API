package aistudio

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"testing"
)

// --- sparseJSONReader ---

func TestSparseJSONReaderPassthrough(t *testing.T) {
	input := `[[[["model","text"],"role",null,7],"x"],"y"]`
	reader := newSparseJSONReader(bytes.NewReader([]byte(input)))
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(decoded) != input {
		t.Fatalf("透传内容不一致:\n got: %s\nwant: %s", decoded, input)
	}
}

func TestSparseJSONReaderInsertsNull(t *testing.T) {
	// 字符串内的逗号/括号不能触发稀疏插入,必须严格透传
	input := `["a,,b,c]"]`
	reader := newSparseJSONReader(bytes.NewReader([]byte(input)))
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(decoded) != input {
		t.Fatalf("字符串内容应透传:\n got: %s\nwant: %s", decoded, input)
	}
	// 字符串外的连续逗号应插入 null:["x",,] → ["x",null,null]
	reader2 := newSparseJSONReader(bytes.NewReader([]byte(`["x",,]`)))
	decoder := json.NewDecoder(reader2)
	var value []any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if len(value) != 3 || value[1] != nil || value[2] != nil {
		t.Fatalf("稀疏插入结果不对: %#v", value)
	}
}

func TestSparseJSONReaderSparseArrayNulls(t *testing.T) {
	// 真实 AI Studio 稀疏场景: [1,,3] 与 [1,2,] 应插入 null
	// [1,,3] 中的 ',,' → "null," ;末尾 ',]' → "null]"
	input := `[[[0],[1,,3,],]]`
	reader := newSparseJSONReader(bytes.NewReader([]byte(input)))
	decoder := json.NewDecoder(reader)
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	inner := value.([]any)[0].([]any)
	// [ [0], [1,null,3,null], null ]
	if len(inner) != 3 {
		t.Fatalf("长度不对: %d", len(inner))
	}
	second := inner[1].([]any)
	if len(second) != 4 || second[1] != nil || second[3] != nil {
		t.Fatalf("稀疏插入结果不对: %#v", second)
	}
	if inner[2] != nil {
		t.Fatalf("末尾 null 插入结果不对: %#v", inner[2])
	}
}

// --- raw 辅助函数 ---

func TestRawInt64(t *testing.T) {
	cases := []struct {
		raw    string
		want   int64
		wantEr bool
	}{
		{"123", 123, false},
		{" 123 ", 123, false},
		{"-45", -45, false},
		{"0", 0, false},
		{"1.5", 0, true},
		{"null", 0, true},
		{`"123"`, 0, true},
		{"true", 0, true},
		{"", 0, true},
	}
	for _, c := range cases {
		got, err := rawInt64(json.RawMessage(c.raw), "$", nil)
		if c.wantEr {
			if err == nil {
				t.Fatalf("rawInt64(%q) 应报错,得到 %d", c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("rawInt64(%q) 意外报错: %v", c.raw, err)
		}
		if got != c.want {
			t.Fatalf("rawInt64(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

func TestRawFloat64(t *testing.T) {
	cases := []struct {
		raw    string
		want   float64
		wantEr bool
	}{
		{"1.5", 1.5, false},
		{" 2.25 ", 2.25, false},
		{"-3", -3, false},
		{"1e3", 1000, false},
		{"null", 0, true},
		{`"1.5"`, 0, true},
		{"true", 0, true},
	}
	for _, c := range cases {
		got, err := rawFloat64(json.RawMessage(c.raw), "$", nil)
		if c.wantEr {
			if err == nil {
				t.Fatalf("rawFloat64(%q) 应报错,得到 %v", c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("rawFloat64(%q) 意外报错: %v", c.raw, err)
		}
		if got != c.want {
			t.Fatalf("rawFloat64(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestRawArray(t *testing.T) {
	values, err := rawArray(json.RawMessage(`[1,"a",null]`), "$", nil)
	if err != nil {
		t.Fatalf("rawArray 报错: %v", err)
	}
	if len(values) != 3 {
		t.Fatalf("rawArray 长度: %d", len(values))
	}
	if _, err := rawArray(json.RawMessage(`{"a":1}`), "$", nil); err == nil {
		t.Fatal("rawArray 对对象应报错")
	}
	// null 解析为 nil 切片且不报错(与旧实现 json.Decoder 行为一致)
	if values, err := rawArray(json.RawMessage(`null`), "$", nil); err != nil || values != nil {
		t.Fatalf("rawArray 对 null 应返回 (nil, nil),得到 (%v, %v)", values, err)
	}
}

// --- decodeGenerateItems / FrameDecoder 端到端 ---

// textFrame 构造一个含文本 part 的最小合法流帧。
// 结构: frame=[candidates=[candidate=[content=[parts=[part=[null,"text"]],"model"]]]]
func textFrame(text string) string {
	return `[[[[[[null,"` + text + `"]],"model"]]]]`
}

func TestDecodeGenerateItemsWithFrameDecoder(t *testing.T) {
	// 根数组 field 1 为 repeated 帧序列
	stream := `[[` + textFrame("hello") + `,` + textFrame("world") + `]]`
	source := bytes.NewReader([]byte(stream))
	decoder := NewFrameDecoder()
	var texts []string
	err := DecodeGenerateStream(source, decoder, func(event Event) error {
		if event.Kind == EventText {
			texts = append(texts, event.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("流解码失败: %v", err)
	}
	if len(texts) != 2 || texts[0] != "hello" || texts[1] != "world" {
		t.Fatalf("事件文本不符合预期: %v", texts)
	}
	// 流结束但未出现完成帧 → End 应报协议错误(并携带 lastFrame 证据)
	if err := decoder.End(); err == nil {
		t.Fatal("无完成帧时 End 应报错")
	}
}

// --- mergeModels ---

func sampleModel(id string, methods []string, caps map[string]bool, options map[string][]string, modes []int64, paid bool) Model {
	return Model{
		ID: id, Name: "n-" + id, Description: "d-" + id,
		Methods: methods, Capabilities: caps, CapabilityOptions: options,
		AccessModes: modes, Paid: paid,
	}
}

func TestMergeModelsSemantics(t *testing.T) {
	base := []Model{
		sampleModel("m1", []string{"a"}, map[string]bool{"x": true}, map[string][]string{"opt": {"v1"}}, []int64{1}, false),
		sampleModel("m2", nil, nil, nil, nil, false),
	}
	additions := []Model{
		sampleModel("m1", []string{"b"}, map[string]bool{"y": true}, map[string][]string{"opt": {"v2"}, "new": {"n1"}}, []int64{2}, true),
		sampleModel("m3", []string{"c"}, nil, nil, nil, false),
	}
	result := mergeModels(base, additions)
	if len(result) != 3 {
		t.Fatalf("模型数量: %d", len(result))
	}
	// 排序后 m1,m2,m3
	m1 := result[0]
	if !m1.Paid {
		t.Fatal("m1 Paid 应合并为 true")
	}
	if !reflect.DeepEqual(m1.Methods, []string{"a", "b"}) {
		t.Fatalf("m1 Methods: %v", m1.Methods)
	}
	if !m1.Capabilities["x"] || !m1.Capabilities["y"] {
		t.Fatalf("m1 Capabilities: %v", m1.Capabilities)
	}
	if !reflect.DeepEqual(m1.CapabilityOptions["opt"], []string{"v1", "v2"}) {
		t.Fatalf("m1 opt: %v", m1.CapabilityOptions["opt"])
	}
	if !reflect.DeepEqual(m1.AccessModes, []int64{1, 2}) {
		t.Fatalf("m1 AccessModes: %v", m1.AccessModes)
	}
}

func TestMergeModelsDoesNotMutateInputs(t *testing.T) {
	baseCaps := map[string]bool{"x": true}
	baseMethods := []string{"a"}
	base := []Model{sampleModel("m1", baseMethods, baseCaps, nil, []int64{1}, false)}
	addCaps := map[string]bool{"y": true}
	additions := []Model{sampleModel("m1", []string{"b"}, addCaps, nil, []int64{2}, false)}

	result := mergeModels(base, additions)

	// 输入的 map/slice 不应被修改
	if len(baseCaps) != 1 || !baseCaps["x"] {
		t.Fatalf("base Capabilities 被污染: %v", baseCaps)
	}
	if len(baseMethods) != 1 || baseMethods[0] != "a" {
		t.Fatalf("base Methods 被污染: %v", baseMethods)
	}
	if len(addCaps) != 1 || !addCaps["y"] {
		t.Fatalf("additions Capabilities 被污染: %v", addCaps)
	}

	// 结果与输入不共享底层数据:修改结果不影响输入
	result[0].Capabilities["z"] = true
	result[0].Methods[0] = "mutated"
	if baseCaps["z"] || baseMethods[0] != "a" {
		t.Fatal("结果与输入共享了底层数据")
	}
}

func TestMergeModelsIntoAccumulate(t *testing.T) {
	merged := make(map[string]Model)
	for index := 0; index < 5; index++ {
		entry := sampleModel("shared", []string{string(rune('a' + index))}, nil, nil, []int64{int64(index)}, index == 3)
		mergeModelsInto(merged, []Model{entry})
	}
	result := materializeMergedModels(merged)
	if len(result) != 1 {
		t.Fatalf("合并后模型数: %d", len(result))
	}
	if len(result[0].Methods) != 5 || len(result[0].AccessModes) != 5 {
		t.Fatalf("增量合并结果不全: Methods=%v AccessModes=%v", result[0].Methods, result[0].AccessModes)
	}
	if !result[0].Paid {
		t.Fatal("Paid 增量合并失败")
	}
}

// --- 基准:验证稀疏读取器零分配 ---

func BenchmarkSparseJSONReaderLargeStream(b *testing.B) {
	// 构造 ~1MB 的典型流式响应
	var buffer bytes.Buffer
	buffer.WriteString(`[0,[`)
	for i := 0; i < 20000; i++ {
		if i > 0 {
			buffer.WriteByte(',')
		}
		buffer.WriteString(`[[["model","hello world this is stream text"],"user"]]`)
	}
	buffer.WriteString(`],7]`)
	input := buffer.Bytes()
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reader := newSparseJSONReader(bytes.NewReader(input))
		if _, err := io.ReadAll(reader); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMergeModelsManyAccounts(b *testing.B) {
	// 50 账户 × 300 模型,与生产规模对齐
	accounts := make([][]Model, 50)
	for a := range accounts {
		account := make([]Model, 300)
		for m := range account {
			account[m] = sampleModel(
				"model-"+string(rune('a'+m%26))+string(rune('a'+m/26%26))+string(rune('a'+m/676%26))+"-"+string(rune('0'+a%10)),
				[]string{"generate", "count"},
				map[string]bool{"tool": true},
				nil, []int64{1, 2}, false,
			)
		}
		accounts[a] = account
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		merged := make(map[string]Model)
		for _, account := range accounts {
			mergeModelsInto(merged, account)
		}
		_ = materializeMergedModels(merged)
	}
}
