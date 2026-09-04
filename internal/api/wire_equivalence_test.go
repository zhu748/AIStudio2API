package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// wire 等价性测试(M2 回归网):
// 旧实现用 map[string]any 构建响应,新实现改为 struct。
// 两者的 JSON 必须逐键等价——包括 "键存在但值为 null/空串" 与 "键不存在" 的区别,
// 因此比较发生在 unmarshal 后的 map[string]any 层(DeepEqual 能区分缺键与 nil 值)。

func jsonOf(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	return out
}

func assertWireEqual(t *testing.T, name string, want any, got any) {
	t.Helper()
	wantMap, gotMap := jsonOf(t, want), jsonOf(t, got)
	if !reflect.DeepEqual(wantMap, gotMap) {
		wantJSON, _ := json.MarshalIndent(wantMap, "", "  ")
		gotJSON, _ := json.MarshalIndent(gotMap, "", "  ")
		t.Errorf("%s wire 不等价:\n--- 旧 map ---\n%s\n--- 新 struct ---\n%s", name, wantJSON, gotJSON)
	}
}

// richResult 构造覆盖所有输出形态的生成结果
func richResult() generationResult {
	textEvent := aistudio.Event{Kind: aistudio.EventText, Text: "hello"}
	emptyText := aistudio.Event{Kind: aistudio.EventText, Text: ""}
	reasoning := aistudio.Event{Kind: aistudio.EventReasoning, Text: "think", ThoughtSignature: "sig-1"}
	sigEvent := aistudio.Event{Kind: aistudio.EventThoughtSignature, ThoughtSignature: "sig-2"}
	tool := aistudio.Event{Kind: aistudio.EventToolCall, ToolCall: &aistudio.FunctionCall{
		ID: "call-1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"NYC"}`), ThoughtSignature: "sig-tool",
	}}
	return generationResult{
		events:        []aistudio.Event{textEvent, emptyText, reasoning, sigEvent, tool},
		toolCalls:     []aistudio.FunctionCall{{ID: "call-1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"NYC"}`), ThoughtSignature: "sig-tool"}},
		citations:     []aistudio.Citation{{URL: "https://example.com", Title: "Example", Start: 3, End: 9}},
		finishReason:  "stop",
		providerModel: "gemini-3.8-flash",
		usage:         &aistudio.Usage{InputTokens: 11, OutputTokens: 22, ReasoningTokens: 5, ToolTokens: 2, TotalTokens: 40},
	}
}

func toolOnlyResult() generationResult {
	return generationResult{
		events:       []aistudio.Event{{Kind: aistudio.EventToolCall, ToolCall: &aistudio.FunctionCall{ID: "c", Name: "f", Arguments: json.RawMessage(`{}`)}}},
		toolCalls:    []aistudio.FunctionCall{{ID: "c", Name: "f", Arguments: json.RawMessage(`{}`)}},
		finishReason: "stop",
	}
}

func TestOpenAICompletionWireEquivalence(t *testing.T) {
	cases := map[string]generationResult{
		"rich":      richResult(),
		"tool-only": toolOnlyResult(),
		"empty":     {},
	}
	for name, result := range cases {
		want := openAICompletionReference("id-1", 12345, "m", result)
		got := buildChatCompletion("id-1", 12345, "m", result)
		assertWireEqual(t, "openai completion "+name, want, got)
	}
}

// openAICompletionReference 是旧 map 实现的完整复刻
func openAICompletionReference(id string, created int64, model string, result generationResult) map[string]any {
	rendered := renderedContent(result.events)
	content := any(rendered)
	if rendered == "" && len(result.toolCalls) > 0 {
		content = nil
	}
	message := map[string]any{"role": "assistant", "content": content}
	if result.reasoning.Len() > 0 {
		message["reasoning_content"] = result.reasoning.String()
	}
	if len(result.toolCalls) > 0 {
		message["tool_calls"] = openAIToolCallReference(result.toolCalls)
	}
	if len(result.citations) > 0 {
		message["annotations"] = openAICitationsReference(result.citations)
	}
	choice := map[string]any{
		"index":         0,
		"message":       message,
		"finish_reason": openAIFinishReason(result.finishReason, len(result.toolCalls) > 0),
	}
	if providerReason := providerFinishReason(result.finishReason); providerReason != "" {
		choice["provider_finish_reason"] = providerReason
	}
	response := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{choice},
	}
	if result.providerModel != "" {
		response["provider_model"] = result.providerModel
	}
	if result.usage != nil {
		response["usage"] = openAIUsageReference(result.usage)
	}
	return response
}

func openAIToolCallReference(calls []aistudio.FunctionCall) []map[string]any {
	output := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		item := map[string]any{
			"id":   call.ID,
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": string(call.Arguments),
			},
		}
		if call.ThoughtSignature != "" {
			item["extra_content"] = map[string]any{"google": map[string]string{"thought_signature": call.ThoughtSignature}}
		}
		output = append(output, item)
	}
	return output
}

func openAICitationsReference(citations []aistudio.Citation) []map[string]any {
	output := make([]map[string]any, 0, len(citations))
	for _, citation := range citations {
		output = append(output, map[string]any{
			"type": "url_citation",
			"url_citation": map[string]any{
				"start_index": citation.Start,
				"end_index":   citation.End,
				"title":       citation.Title,
				"url":         citation.URL,
			},
		})
	}
	return output
}

func openAIUsageReference(usage *aistudio.Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":     inputTokens(usage),
		"completion_tokens": outputTokens(usage),
		"total_tokens":      usage.TotalTokens,
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": usage.ReasoningTokens,
		},
	}
}

func TestOpenAIChunkWireEquivalence(t *testing.T) {
	finish := "stop"
	text := "hi"
	cases := []struct {
		name         string
		delta        map[string]any
		newDelta     openAIDeltaOut
		finish       *string
		includeUsage bool
		provider     string
	}{
		{"role+usage", map[string]any{"role": "assistant", "content": ""}, openAIDeltaOut{Role: "assistant"}, nil, true, ""},
		{"text", map[string]any{"content": text}, openAIDeltaOut{Content: &text}, nil, false, ""},
		{"finish", map[string]any{}, openAIDeltaOut{}, &finish, true, "provider_stop"},
		{"no-usage", map[string]any{}, openAIDeltaOut{}, nil, false, ""},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		if err := writeChatChunkReference(recorder, "id-1", 123, "m", tc.delta, tc.finish, tc.includeUsage, tc.provider); err != nil {
			t.Fatal(err)
		}
		want := extractSSEData(t, recorder)
		recorder2 := httptest.NewRecorder()
		newDelta := tc.newDelta
		if tc.name == "role+usage" {
			empty := ""
			newDelta.Content = &empty
		}
		if err := writeChatChunk(recorder2, "id-1", 123, "m", newDelta, tc.finish, tc.includeUsage, tc.provider); err != nil {
			t.Fatal(err)
		}
		got := extractSSEData(t, recorder2)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s chunk 不等价:\nwant=%v\ngot=%v", tc.name, want, got)
		}
	}
}

// writeChatChunkReference 旧 map 实现复刻(含 provider_finish_reason 上提与 usage:null)
func writeChatChunkReference(w http.ResponseWriter, id string, created int64, model string, delta map[string]any, finish *string, includeUsage bool, providerReason string) error {
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": finish,
	}
	if providerReason != "" {
		choice["provider_finish_reason"] = providerReason
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{choice},
	}
	if includeUsage {
		chunk["usage"] = nil
	}
	return writeSSE(w, "", chunk)
}

func extractSSEData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := recorder.Body.String()
	prefix := "data: "
	start := bytes.Index([]byte(body), []byte(prefix))
	if start < 0 {
		t.Fatalf("SSE 帧缺少 data 前缀: %q", body)
	}
	rest := body[start+len(prefix):]
	end := bytes.Index([]byte(rest), []byte("\n\n"))
	if end < 0 {
		t.Fatalf("SSE 帧缺少结尾: %q", body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(rest[:end]), &out); err != nil {
		t.Fatalf("unmarshal %q: %v", rest[:end], err)
	}
	return out
}

func TestAnthropicResponseWireEquivalence(t *testing.T) {
	cases := map[string]generationResult{
		"rich":      richResult(),
		"tool-only": toolOnlyResult(),
		"empty":     {},
	}
	for name, result := range cases {
		want := anthropicResponseReference("msg-1", "m", result)
		got := buildAnthropicResponse("msg-1", "m", result)
		assertWireEqual(t, "anthropic message "+name, want, got)
	}
}

func anthropicResponseReference(id string, model string, result generationResult) map[string]any {
	stopReason, stopSequence := anthropicStop(result.finishReason, len(result.toolCalls) > 0, result.stopSequence)
	response := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       anthropicBlocks(result),
		"stop_reason":   stopReason,
		"stop_sequence": stopSequence,
	}
	if result.providerModel != "" {
		response["provider_model"] = result.providerModel
	}
	if providerReason := providerFinishReason(result.finishReason); providerReason != "" {
		response["provider_finish_reason"] = providerReason
	}
	if result.usage != nil {
		response["usage"] = map[string]any{
			"input_tokens":  inputTokens(result.usage),
			"output_tokens": outputTokens(result.usage),
		}
	}
	return response
}

func TestAnthropicStreamEventsWireEquivalence(t *testing.T) {
	// message_start
	assertWireEqual(t, "message_start",
		map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg-1", "type": "message", "role": "assistant", "model": "m",
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]int64{"input_tokens": 7, "output_tokens": 0},
			},
		},
		anthropicMessageStartEvent{
			Type: "message_start",
			Message: anthropicStreamMessage{
				ID: "msg-1", Type: "message", Role: "assistant", Model: "m",
				Content: []anthropicContentBlock{}, StopReason: nil, StopSequence: nil,
				Usage: anthropicUsageBody{InputTokens: 7, OutputTokens: 0},
			},
		})

	// text_delta 空串与文本
	for _, text := range []string{"", "hello"} {
		assertWireEqual(t, "text_delta "+text,
			map[string]any{"type": "content_block_delta", "index": 2, "delta": map[string]any{"type": "text_delta", "text": text}},
			anthropicBlockDeltaEvent{Type: "content_block_delta", Index: 2, Delta: anthropicDeltaOut{Type: "text_delta", Text: &text}})
	}

	// thinking_delta
	thinking := "ponder"
	assertWireEqual(t, "thinking_delta",
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": thinking}},
		anthropicBlockDeltaEvent{Type: "content_block_delta", Index: 0, Delta: anthropicDeltaOut{Type: "thinking_delta", Thinking: &thinking}})

	// tool_use 起始块(input 为 {})
	assertWireEqual(t, "tool_use start",
		map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "c1", "name": "f", "input": map[string]any{}}},
		anthropicBlockStartEvent{Type: "content_block_start", Index: 1, ContentBlock: anthropicBlockOut{Type: "tool_use", ID: "c1", Name: "f", Input: json.RawMessage("{}")}})

	// input_json_delta(空 arguments)
	partial := ""
	assertWireEqual(t, "input_json_delta empty",
		map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": ""}},
		anthropicBlockDeltaEvent{Type: "content_block_delta", Index: 1, Delta: anthropicDeltaOut{Type: "input_json_delta", PartialJSON: &partial}})

	// signature_delta / redacted_thinking / block_stop
	signature := "sig"
	assertWireEqual(t, "signature_delta",
		map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "signature_delta", "signature": signature}},
		anthropicBlockDeltaEvent{Type: "content_block_delta", Index: 0, Delta: anthropicDeltaOut{Type: "signature_delta", Signature: &signature}})
	assertWireEqual(t, "redacted_thinking start",
		map[string]any{"type": "content_block_start", "index": 3, "content_block": map[string]any{"type": "redacted_thinking", "data": "data-1"}},
		anthropicBlockStartEvent{Type: "content_block_start", Index: 3, ContentBlock: anthropicBlockOut{Type: "redacted_thinking", Data: "data-1"}})
	assertWireEqual(t, "block_stop",
		map[string]any{"type": "content_block_stop", "index": 3},
		anthropicBlockStopEvent{Type: "content_block_stop", Index: 3})

	// text/thinking 起始块(空串字段必须输出)
	textEmpty := ""
	assertWireEqual(t, "text start",
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
		anthropicBlockStartEvent{Type: "content_block_start", Index: 0, ContentBlock: anthropicBlockOut{Type: "text", Text: &textEmpty}})
	thinkEmpty := ""
	thinkBlock := anthropicBlockOut{Type: "thinking"}
	thinkBlock.Thinking = &thinkEmpty
	assertWireEqual(t, "thinking start",
		map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": ""}},
		anthropicBlockStartEvent{Type: "content_block_start", Index: 0, ContentBlock: thinkBlock})

	// message_delta: usage 有/无
	assertWireEqual(t, "message_delta no usage",
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int64{"output_tokens": 0}},
		anthropicMessageDeltaEvent{Type: "message_delta", Delta: anthropicMessageDeltaBody{StopReason: "end_turn"}, Usage: anthropicUsageDeltaOut{}})
	assertWireEqual(t, "message_delta with usage",
		map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil, "provider_finish_reason": "provider_x"},
			"usage": map[string]any{"input_tokens": 9, "output_tokens": 4},
		},
		anthropicMessageDeltaEvent{
			Type:  "message_delta",
			Delta: anthropicMessageDeltaBody{StopReason: "tool_use", ProviderFinishReason: "provider_x"},
			Usage: anthropicUsageDeltaOut{InputTokens: &[]int64{9}[0], OutputTokens: 4},
		})
	// message_delta usage input=0 也必须输出
	inputZero := int64(0)
	assertWireEqual(t, "message_delta zero input",
		map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}},
		anthropicMessageDeltaEvent{Type: "message_delta", Delta: anthropicMessageDeltaBody{StopReason: "end_turn"}, Usage: anthropicUsageDeltaOut{InputTokens: &inputZero, OutputTokens: 0}})

	// message_stop / error
	assertWireEqual(t, "message_stop", map[string]string{"type": "message_stop"}, anthropicMessageStopEvent{Type: "message_stop"})
}

func TestGeminiResponseWireEquivalence(t *testing.T) {
	cases := map[string]generationResult{
		"rich":      richResult(),
		"tool-only": toolOnlyResult(),
		"empty":     {},
	}
	request := aistudio.GenerateRequest{ID: "resp-1", Model: "m"}
	for name, result := range cases {
		want := geminiResponseReference(request, result)
		got := buildGeminiResponse(request, result)
		assertWireEqual(t, "gemini response "+name, want, got)
	}

	// grounding 变体
	grounding := richResult()
	grounding.grounding = &aistudio.GroundingMetadata{
		SearchEntryPoint: &aistudio.SearchEntryPoint{RenderedContent: "<x>"},
		Chunks: []aistudio.GroundingChunk{
			{Source: "web", URI: "u1", Title: "t1"},
			{Source: "retrieved_context", URI: "u2", Title: "t2", Text: "c2"},
			{Source: "maps", URI: "u3", Title: "t3", Text: "c3", PlaceID: "p3"},
		},
		Supports: []aistudio.GroundingSupport{
			{Segment: aistudio.GroundingSegment{PartIndex: 0, StartIndex: 1, EndIndex: 5, Text: "seg"}, ChunkIndices: []int{0, 2}, ConfidenceScores: []float64{0.5, 0.7}},
			{Segment: aistudio.GroundingSegment{}, ChunkIndices: nil},
		},
		DynamicRetrievalScore:  &[]float64{0.42}[0],
		WebSearchQueries:       []string{"q1"},
		MapsWidgetContextToken: "token-1",
	}
	want := geminiResponseReference(request, grounding)
	got := buildGeminiResponse(request, grounding)
	assertWireEqual(t, "gemini grounding", want, got)

	// 空搜索入口(entry 输出 {} 而非缺键)
	grounding.grounding = &aistudio.GroundingMetadata{SearchEntryPoint: &aistudio.SearchEntryPoint{}}
	assertWireEqual(t, "gemini empty entry", geminiResponseReference(request, grounding), buildGeminiResponse(request, grounding))

	// countTokens / models
	assertWireEqual(t, "countTokens", map[string]int64{"totalTokens": 42}, geminiCountTokensOut{TotalTokens: 42})
	model := aistudio.Model{ID: "m1", Name: "Model 1", Description: "d", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"x": true}, Paid: true}
	assertWireEqual(t, "model object", geminiModelReference(model), geminiModelObject(model))
}

func geminiResponseReference(request aistudio.GenerateRequest, result generationResult) map[string]any {
	candidate := map[string]any{
		"content": map[string]any{"role": "model", "parts": geminiPartsReference(result)},
		"index":   0,
	}
	setGeminiFinishReference(candidate, result.finishReason)
	if result.grounding != nil {
		candidate["groundingMetadata"] = geminiGroundingReference(*result.grounding)
	} else if len(result.citations) > 0 {
		candidate["citationMetadata"] = geminiCitationsReference(result.citations)
	}
	response := map[string]any{
		"candidates":   []any{candidate},
		"modelVersion": request.Model,
		"responseId":   request.ID,
	}
	if result.providerModel != "" {
		response["modelVersion"] = result.providerModel
	}
	if result.usage != nil {
		response["usageMetadata"] = map[string]any{
			"promptTokenCount":        result.usage.InputTokens,
			"candidatesTokenCount":    result.usage.OutputTokens,
			"thoughtsTokenCount":      result.usage.ReasoningTokens,
			"toolUsePromptTokenCount": result.usage.ToolTokens,
			"totalTokenCount":         result.usage.TotalTokens,
		}
	}
	return response
}

func setGeminiFinishReference(candidate map[string]any, reason string) {
	candidate["finishReason"] = geminiFinishReason(reason)
	normalized := lowerTrim(reason)
	if normalized == "missing_thought_signature" {
		candidate["finishMessage"] = "Missing thought signature"
	} else if len(normalized) > 8 && normalized[:9] == "provider_" {
		candidate["finishMessage"] = "AI Studio finish reason " + normalized[9:]
	}
}

func lowerTrim(value string) string {
	out := make([]byte, 0, len(value))
	for _, char := range value {
		if char >= 'A' && char <= 'Z' {
			char += 'a' - 'A'
		}
		if char != ' ' && char != '\t' {
			out = append(out, byte(char))
		}
	}
	return string(out)
}

func geminiPartsReference(result generationResult) []map[string]any {
	parts := make([]map[string]any, 0)
	for _, event := range result.events {
		switch event.Kind {
		case aistudio.EventText:
			part := map[string]any{"text": event.Text}
			if event.ThoughtSignature != "" {
				part["thoughtSignature"] = event.ThoughtSignature
			}
			parts = append(parts, part)
		case aistudio.EventReasoning:
			part := map[string]any{"text": event.Text, "thought": true}
			if event.ThoughtSignature != "" {
				part["thoughtSignature"] = event.ThoughtSignature
			}
			parts = append(parts, part)
		case aistudio.EventToolCall:
			if event.ToolCall == nil {
				continue
			}
			part := map[string]any{"functionCall": map[string]any{
				"id": event.ToolCall.ID, "name": event.ToolCall.Name, "args": event.ToolCall.Arguments,
			}}
			if event.ToolCall.ThoughtSignature != "" {
				part["thoughtSignature"] = event.ToolCall.ThoughtSignature
			}
			parts = append(parts, part)
		case aistudio.EventThoughtSignature:
			if event.ThoughtSignature != "" {
				parts = append(parts, map[string]any{"thoughtSignature": event.ThoughtSignature})
			}
		}
	}
	return parts
}

func geminiCitationsReference(citations []aistudio.Citation) map[string]any {
	sources := make([]map[string]any, 0, len(citations))
	for _, citation := range citations {
		sources = append(sources, map[string]any{
			"uri": citation.URL, "title": citation.Title, "startIndex": citation.Start, "endIndex": citation.End,
		})
	}
	return map[string]any{"citationSources": sources}
}

func geminiGroundingReference(metadata aistudio.GroundingMetadata) map[string]any {
	output := map[string]any{}
	if metadata.SearchEntryPoint != nil {
		entry := map[string]any{}
		if metadata.SearchEntryPoint.RenderedContent != "" {
			entry["renderedContent"] = metadata.SearchEntryPoint.RenderedContent
		}
		if metadata.SearchEntryPoint.SDKBlob != "" {
			entry["sdkBlob"] = metadata.SearchEntryPoint.SDKBlob
		}
		output["searchEntryPoint"] = entry
	}
	if len(metadata.Chunks) > 0 {
		chunks := make([]map[string]any, 0, len(metadata.Chunks))
		for _, chunk := range metadata.Chunks {
			value := map[string]any{"uri": chunk.URI, "title": chunk.Title}
			switch chunk.Source {
			case "web":
				chunks = append(chunks, map[string]any{"web": value})
			case "retrieved_context":
				value["text"] = chunk.Text
				chunks = append(chunks, map[string]any{"retrievedContext": value})
			case "maps":
				value["text"] = chunk.Text
				value["placeId"] = chunk.PlaceID
				chunks = append(chunks, map[string]any{"maps": value})
			}
		}
		output["groundingChunks"] = chunks
	}
	if len(metadata.Supports) > 0 {
		supports := make([]map[string]any, 0, len(metadata.Supports))
		for _, support := range metadata.Supports {
			value := map[string]any{
				"segment": map[string]any{
					"partIndex": support.Segment.PartIndex, "startIndex": support.Segment.StartIndex,
					"endIndex": support.Segment.EndIndex, "text": support.Segment.Text,
				},
				"groundingChunkIndices": support.ChunkIndices,
			}
			if len(support.ConfidenceScores) > 0 {
				value["confidenceScores"] = support.ConfidenceScores
			}
			supports = append(supports, value)
		}
		output["groundingSupports"] = supports
	}
	if metadata.DynamicRetrievalScore != nil {
		output["retrievalMetadata"] = map[string]any{
			"googleSearchDynamicRetrievalScore": *metadata.DynamicRetrievalScore,
		}
	}
	if len(metadata.WebSearchQueries) > 0 {
		output["webSearchQueries"] = metadata.WebSearchQueries
	}
	if metadata.MapsWidgetContextToken != "" {
		output["googleMapsWidgetContextToken"] = metadata.MapsWidgetContextToken
	}
	return output
}

func geminiModelReference(model aistudio.Model) map[string]any {
	item := map[string]any{
		"name":                       "models/" + model.ID,
		"displayName":                model.Name,
		"description":                model.Description,
		"supportedGenerationMethods": model.Methods,
		"inputTokenLimit":            model.InputTokenLimit,
		"outputTokenLimit":           model.OutputTokenLimit,
	}
	if len(model.Capabilities) > 0 {
		item["capabilities"] = model.Capabilities
	}
	if len(model.CapabilityOptions) > 0 {
		item["capabilityOptions"] = model.CapabilityOptions
	}
	if len(model.AccessModes) > 0 {
		item["accessModes"] = model.AccessModes
	}
	if model.Paid {
		item["paid"] = true
	}
	return item
}

func TestGeminiStreamChunkWireEquivalence(t *testing.T) {
	request := aistudio.GenerateRequest{ID: "resp-1", Model: "m"}

	text := "hi"
	sig := "sig"
	part := geminiPartOut{Text: &text, ThoughtSignature: sig}
	got := geminiStreamChunkOut{
		ResponseID: request.ID, ModelVersion: request.Model,
		Candidates: []geminiCandidateOut{geminiStreamCandidate(part)},
	}
	want := map[string]any{
		"responseId": request.ID, "modelVersion": request.Model,
		"candidates": []any{map[string]any{
			"index":   0,
			"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text, "thoughtSignature": sig}}},
		}},
	}
	assertWireEqual(t, "gemini stream text frame", want, got)

	// thought 独立签名帧
	gotSig := geminiStreamChunkOut{
		ResponseID: request.ID, ModelVersion: request.Model,
		Candidates: []geminiCandidateOut{geminiStreamCandidate(geminiPartOut{ThoughtSignature: sig})},
	}
	wantSig := map[string]any{
		"responseId": request.ID, "modelVersion": request.Model,
		"candidates": []any{map[string]any{
			"index":   0,
			"content": map[string]any{"role": "model", "parts": []any{map[string]any{"thoughtSignature": sig}}},
		}},
	}
	assertWireEqual(t, "gemini stream sig frame", wantSig, gotSig)

	// 空文本帧: text="" 必须输出
	empty := ""
	gotEmpty := geminiStreamChunkOut{
		ResponseID: request.ID, ModelVersion: request.Model,
		Candidates: []geminiCandidateOut{geminiStreamCandidate(geminiPartOut{Text: &empty})},
	}
	wantEmpty := map[string]any{
		"responseId": request.ID, "modelVersion": request.Model,
		"candidates": []any{map[string]any{
			"index":   0,
			"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": ""}}},
		}},
	}
	assertWireEqual(t, "gemini stream empty text", wantEmpty, gotEmpty)

	// 收尾帧: usage + finishReason
	final := geminiStreamChunkOut{
		ResponseID: request.ID, ModelVersion: request.Model,
		Candidates: []geminiCandidateOut{{Index: 0}},
	}
	setGeminiFinish(&final.Candidates[0], "stop")
	final.UsageMetadata = geminiUsage(&aistudio.Usage{InputTokens: 3, OutputTokens: 4, TotalTokens: 7})
	wantFinal := map[string]any{
		"responseId": request.ID, "modelVersion": request.Model,
		"candidates": []any{map[string]any{"index": 0, "finishReason": "STOP"}},
		"usageMetadata": map[string]any{
			"promptTokenCount":        int64(3),
			"candidatesTokenCount":    int64(4),
			"thoughtsTokenCount":      int64(0),
			"toolUsePromptTokenCount": int64(0),
			"totalTokenCount":         int64(7),
		},
	}
	assertWireEqual(t, "gemini final frame", wantFinal, final)

	// 错误帧
	assertWireEqual(t, "gemini error frame",
		map[string]any{"error": map[string]any{"code": 502, "message": "boom", "status": "INTERNAL"}},
		geminiStreamErrorOut{Error: geminiErrorInfoOut{Code: 502, Message: "boom", Status: "INTERNAL"}})
}

// 基准:量化 struct 相对 map 的序列化收益(模拟 200 token 的流式 chunk)
func BenchmarkOpenAIChunkMarshal(b *testing.B) {
	text := "token piece"
	finish := "stop"
	b.Run("map", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			chunk := map[string]any{
				"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": int64(1), "model": "m",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}},
				"usage":   nil,
			}
			if _, err := json.Marshal(chunk); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("struct", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			delta := openAIDeltaOut{Content: &text}
			chunk := openAIChunkOut{
				ID: "chatcmpl-1", Object: "chat.completion.chunk", Created: 1, Model: "m",
				Choices: []openAIChoiceChunkOut{{Index: 0, Delta: delta, FinishReason: &finish}},
				Usage:   (*openAIUsageOut)(nil),
			}
			if _, err := json.Marshal(chunk); err != nil {
				b.Fatal(err)
			}
		}
	})
}
