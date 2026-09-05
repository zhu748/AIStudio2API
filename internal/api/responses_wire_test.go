package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// responsesStreamFramesReference 是旧 map emit 实现的逐帧复刻,
// 用于校验 struct 化改造后的帧与旧行为逐键等价(含空串/空数组键)。
func responsesStreamFramesReference() []map[string]any {
	textDelta := func(itemID string, outputIndex int, delta string) map[string]any {
		return map[string]any{
			"type": "response.output_text.delta", "sequence_number": 0,
			"item_id": itemID, "output_index": outputIndex, "content_index": 0,
			"delta": delta, "logprobs": []any{},
		}
	}
	reasoningDelta := func(itemID string, outputIndex int, delta string) map[string]any {
		return map[string]any{
			"type": "response.reasoning_summary_text.delta", "sequence_number": 0,
			"item_id": itemID, "output_index": outputIndex, "summary_index": 0,
			"delta": delta,
		}
	}
	functionDelta := func(itemID string, outputIndex int, delta string) map[string]any {
		return map[string]any{
			"type": "response.function_call_arguments.delta", "sequence_number": 0,
			"item_id": itemID, "output_index": outputIndex, "delta": delta,
		}
	}
	functionDone := func(itemID string, outputIndex int, arguments string, name string) map[string]any {
		return map[string]any{
			"type": "response.function_call_arguments.done", "sequence_number": 0,
			"item_id": itemID, "output_index": outputIndex, "arguments": arguments, "name": name,
		}
	}
	shellEvent := func(eventType string) map[string]any {
		return map[string]any{"type": eventType, "sequence_number": 0, "response": map[string]any{}}
	}
	return []map[string]any{
		shellEvent("response.created"),
		shellEvent("response.in_progress"),
		textDelta("msg_1", 0, "hello"),
		textDelta("msg_1", 0, ""),
		reasoningDelta("rs_1", 1, "think"),
		functionDelta("fc_1", 2, `{"city":"NYC"}`),
		functionDone("fc_1", 2, `{"city":"NYC"}`, "get_weather"),
	}
}

// TestResponsesStreamFramesWireEquivalence 校验 Responses 协议
// struct 化热帧(含本轮新增的双帧信封与 function_call 帧)与旧
// map 帧逐键等价。
func TestResponsesStreamFramesWireEquivalence(t *testing.T) {
	request := responsesRequest{Model: "m"}

	makeWriter := func() *responsesStreamWriter {
		return &responsesStreamWriter{
			w: httptest.NewRecorder(), id: "1", created: 1, request: request,
			indexes: map[string]int{}, sequence: 0,
		}
	}

	// response.created / in_progress 双帧
	for _, eventType := range []string{"response.created", "response.in_progress"} {
		writer := makeWriter()
		want := map[string]any{
			"type": eventType, "sequence_number": 0,
			"response": map[string]any{"model": "m"},
		}
		got := responsesShellEventOut{
			Type: eventType, SequenceNumber: writer.sequence,
			Response: map[string]any{"model": "m"},
		}
		assertWireEqual(t, "responses shell "+eventType, want, got)
		if err := writer.emitResponseShell(eventType, map[string]any{"model": "m"}); err != nil {
			t.Fatalf("emit %s: %v", eventType, err)
		}
		if writer.sequence != 1 {
			t.Errorf("%s 后序号应为 1,实际 %d", eventType, writer.sequence)
		}
	}

	// output_text.delta(空串与文本)
	for _, delta := range []string{"", "hello"} {
		want := map[string]any{
			"type": "response.output_text.delta", "sequence_number": 0,
			"item_id": "msg_1", "output_index": 0, "content_index": 0,
			"delta": delta, "logprobs": []any{},
		}
		got := responsesTextDeltaOut{
			Type: "response.output_text.delta", SequenceNumber: 0,
			ItemID: "msg_1", OutputIndex: 0, ContentIndex: 0, Delta: delta, Logprobs: []any{},
		}
		assertWireEqual(t, "responses text delta "+delta, want, got)
	}

	// reasoning_summary_text.delta
	assertWireEqual(t, "responses reasoning delta",
		map[string]any{
			"type": "response.reasoning_summary_text.delta", "sequence_number": 0,
			"item_id": "rs_1", "output_index": 1, "summary_index": 0, "delta": "think",
		},
		responsesReasoningDeltaOut{
			Type: "response.reasoning_summary_text.delta", SequenceNumber: 0,
			ItemID: "rs_1", OutputIndex: 1, SummaryIndex: 0, Delta: "think",
		})

	// function_call_arguments.delta / done(arguments 为完整 JSON 大帧)
	arguments := `{"city":"NYC"}`
	assertWireEqual(t, "responses function delta",
		map[string]any{
			"type": "response.function_call_arguments.delta", "sequence_number": 0,
			"item_id": "fc_1", "output_index": 2, "delta": arguments,
		},
		responsesFunctionCallDeltaOut{
			Type: "response.function_call_arguments.delta", SequenceNumber: 0,
			ItemID: "fc_1", OutputIndex: 2, Delta: arguments,
		})
	assertWireEqual(t, "responses function done",
		map[string]any{
			"type": "response.function_call_arguments.done", "sequence_number": 0,
			"item_id": "fc_1", "output_index": 2, "arguments": arguments, "name": "get_weather",
		},
		responsesFunctionCallDoneOut{
			Type: "response.function_call_arguments.done", SequenceNumber: 0,
			ItemID: "fc_1", OutputIndex: 2, Arguments: arguments, Name: "get_weather",
		})
}

// TestResponsesShellReusedAcrossFrames 验证双帧共享同一 shell 时
// 序号推进正确且两次 emit 的 SSE 输出均为合法 JSON。
func TestResponsesShellReusedAcrossFrames(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &responsesStreamWriter{
		w: recorder, id: "resp_9", created: 42, request: responsesRequest{Model: "m"},
		indexes: map[string]int{},
	}
	shell := responseShell("resp_9", 42, "in_progress", writer.request)
	if err := writer.emitResponseShell("response.created", shell); err != nil {
		t.Fatalf("emit created: %v", err)
	}
	if err := writer.emitResponseShell("response.in_progress", shell); err != nil {
		t.Fatalf("emit in_progress: %v", err)
	}
	if writer.sequence != 2 {
		t.Fatalf("双帧后序号应为 2,实际 %d", writer.sequence)
	}

	body := recorder.Body.String()
	// shell["created_at"] 为 int,JSON 往返后为 float64,统一转换后比较
	createdAt := float64(42)
	for index, expected := range []struct {
		eventType string
		sequence  int
	}{
		{"response.created", 0},
		{"response.in_progress", 1},
	} {
		var frame struct {
			Type           string         `json:"type"`
			SequenceNumber int            `json:"sequence_number"`
			Response       map[string]any `json:"response"`
		}
		// writeSSE 每帧输出 "event: <type>\ndata: <json>\n\n"。
		lines := splitSSEFrames(body)
		if index >= len(lines) {
			t.Fatalf("帧数量不足: %d", len(lines))
		}
		if err := json.Unmarshal([]byte(lines[index]), &frame); err != nil {
			t.Fatalf("解析第 %d 帧: %v\n%s", index, err, lines[index])
		}
		if frame.Type != expected.eventType {
			t.Errorf("第 %d 帧类型 %s != %s", index, frame.Type, expected.eventType)
		}
		if frame.SequenceNumber != expected.sequence {
			t.Errorf("第 %d 帧序号 %d != %d", index, frame.SequenceNumber, expected.sequence)
		}
		if frame.Response["status"] != "in_progress" {
			t.Errorf("第 %d 帧状态应为 in_progress", index)
		}
		if number, ok := frame.Response["created_at"].(float64); !ok || number != createdAt {
			t.Errorf("第 %d 帧 shell 内容与首次构建不一致: created_at=%v", index, frame.Response["created_at"])
		}
	}
}

// splitSSEFrames 从 SSE 输出中提取 data: 行的 JSON 负载
func splitSSEFrames(body string) []string {
	var frames []string
	current := ""
	for _, line := range splitLines(body) {
		if len(line) > 6 && line[:6] == "data: " {
			current = line[6:]
		} else if line == "" && current != "" {
			frames = append(frames, current)
			current = ""
		}
	}
	if current != "" {
		frames = append(frames, current)
	}
	return frames
}

func splitLines(body string) []string {
	var lines []string
	start := 0
	for index := 0; index < len(body); index++ {
		if body[index] == '\n' {
			lines = append(lines, body[start:index])
			start = index + 1
		}
	}
	if start < len(body) {
		lines = append(lines, body[start:])
	}
	return lines
}
