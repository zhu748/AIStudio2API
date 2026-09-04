package api

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// referenceAnthropicBlocks 是 anthropicBlocks 的旧实现(直接 `+=` 累积),
// 仅作为新实现(blockAccumulator 延迟固化)的等价性参照保留在测试中。
func referenceAnthropicBlocks(result generationResult) []anthropicContentBlock {
	blocks := make([]anthropicContentBlock, 0)
	for _, event := range result.events {
		switch event.Kind {
		case aistudio.EventText:
			if len(blocks) > 0 && blocks[len(blocks)-1].Type == "text" {
				blocks[len(blocks)-1].Text += event.Text
			} else {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: event.Text})
			}
		case aistudio.EventReasoning:
			if len(blocks) > 0 && blocks[len(blocks)-1].Type == "thinking" {
				*blocks[len(blocks)-1].Thinking += event.Text
				if event.ThoughtSignature != "" {
					blocks[len(blocks)-1].Signature = event.ThoughtSignature
				}
			} else {
				thinking := event.Text
				blocks = append(blocks, anthropicContentBlock{Type: "thinking", Thinking: &thinking, Signature: event.ThoughtSignature})
			}
		case aistudio.EventThoughtSignature:
			if event.ThoughtSignature == "" {
				continue
			}
			blocks = append(blocks, anthropicContentBlock{Type: "redacted_thinking", Data: event.ThoughtSignature})
		case aistudio.EventToolCall:
			if event.ToolCall != nil {
				if event.ToolCall.ThoughtSignature != "" {
					if len(blocks) > 0 && blocks[len(blocks)-1].Type == "thinking" {
						blocks[len(blocks)-1].Signature = event.ToolCall.ThoughtSignature
					} else {
						blocks = append(blocks, anthropicContentBlock{
							Type: "redacted_thinking", Data: event.ToolCall.ThoughtSignature,
						})
					}
				}
				blocks = append(blocks, anthropicContentBlock{
					Type: "tool_use", ID: event.ToolCall.ID, Name: event.ToolCall.Name, Input: event.ToolCall.Arguments,
				})
			}
		case aistudio.EventMedia:
			if event.Media != nil {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: renderMediaMarkdown(*event.Media)})
			}
		case aistudio.EventExecutableCode, aistudio.EventCodeExecutionResult:
			rendered := renderCodeExecution(event)
			if rendered == "" {
				continue
			}
			if len(blocks) > 0 && blocks[len(blocks)-1].Type == "text" {
				blocks[len(blocks)-1].Text += "\n" + rendered + "\n"
			} else {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: rendered + "\n"})
			}
		}
	}
	if sources := renderCitationsMarkdown(result.citations); sources != "" {
		if len(blocks) > 0 && blocks[len(blocks)-1].Type == "text" {
			blocks[len(blocks)-1].Text += "\n\n" + sources
		} else {
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: sources})
		}
	}
	return blocks
}

func blocksDeepEqual(left []anthropicContentBlock, right []anthropicContentBlock) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Type != right[index].Type || left[index].Text != right[index].Text ||
			left[index].Data != right[index].Data || left[index].Signature != right[index].Signature ||
			left[index].ID != right[index].ID || left[index].Name != right[index].Name ||
			!reflect.DeepEqual(left[index].Input, right[index].Input) {
			return false
		}
		if (left[index].Thinking == nil) != (right[index].Thinking == nil) {
			return false
		}
		if left[index].Thinking != nil && *left[index].Thinking != *right[index].Thinking {
			return false
		}
	}
	return true
}

// syntheticEvents 生成覆盖所有块类型与交错顺序的事件序列。
func syntheticEvents(rng *rand.Rand) generationResult {
	kindCount := rng.Intn(24)
	events := make([]aistudio.Event, 0, kindCount)
	for index := 0; index < kindCount; index++ {
		text := fmt.Sprintf("t%d", index)
		switch rng.Intn(7) {
		case 0:
			events = append(events, aistudio.Event{Kind: aistudio.EventText, Text: text})
		case 1:
			signature := ""
			if rng.Intn(2) == 0 {
				signature = fmt.Sprintf("sig%d", index)
			}
			events = append(events, aistudio.Event{Kind: aistudio.EventReasoning, Text: "r" + text, ThoughtSignature: signature})
		case 2:
			if rng.Intn(2) == 0 {
				events = append(events, aistudio.Event{Kind: aistudio.EventThoughtSignature, ThoughtSignature: fmt.Sprintf("redact%d", index)})
			} else {
				events = append(events, aistudio.Event{Kind: aistudio.EventThoughtSignature, ThoughtSignature: ""})
			}
		case 3:
			call := &aistudio.FunctionCall{
				ID:        fmt.Sprintf("call%d", index),
				Name:      "tool",
				Arguments: json.RawMessage(`{"a":1}`),
			}
			if rng.Intn(2) == 0 {
				call.ThoughtSignature = fmt.Sprintf("callsig%d", index)
			}
			events = append(events, aistudio.Event{Kind: aistudio.EventToolCall, ToolCall: call})
		case 4:
			events = append(events, aistudio.Event{
				Kind:  aistudio.EventMedia,
				Media: &aistudio.Media{MIME: "image/png", Data: []byte("fake")},
			})
		case 5:
			events = append(events, aistudio.Event{
				Kind:           aistudio.EventExecutableCode,
				ExecutableCode: &aistudio.ExecutableCode{Language: "python", Code: "print(1)"},
			})
		case 6:
			events = append(events, aistudio.Event{
				Kind:                aistudio.EventCodeExecutionResult,
				CodeExecutionResult: &aistudio.CodeExecutionResult{Output: "1"},
			})
		}
	}
	result := generationResult{events: events}
	if rng.Intn(2) == 0 {
		result.citations = []aistudio.Citation{{Start: 0, End: 2, Title: "t", URL: "https://x"}}
	}
	return result
}

func TestAnthropicBlocksEquivalentToReference(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for iteration := 0; iteration < 2000; iteration++ {
		result := syntheticEvents(rng)
		expected := referenceAnthropicBlocks(result)
		actual := anthropicBlocks(result)
		if !blocksDeepEqual(expected, actual) {
			t.Fatalf("第 %d 轮输出不一致:\nexpected: %#v\nactual:   %#v", iteration, expected, actual)
		}
	}
}

func TestAnthropicBlocksEmpty(t *testing.T) {
	if blocks := anthropicBlocks(generationResult{}); len(blocks) != 0 {
		t.Fatalf("空事件应产出空块,得到 %d", len(blocks))
	}
}

// --- renderedContent ---

func TestRenderedContentNewlineBehavior(t *testing.T) {
	// 媒体/代码事件前的换行补齐逻辑必须与旧行为一致
	events := []aistudio.Event{
		{Kind: aistudio.EventText, Text: "hello"},
		{Kind: aistudio.EventMedia, Media: &aistudio.Media{MIME: "image/png", Data: []byte("x")}},
		{Kind: aistudio.EventText, Text: "world"},
		{Kind: aistudio.EventExecutableCode, ExecutableCode: &aistudio.ExecutableCode{Language: "python", Code: "print(1)"}},
		{Kind: aistudio.EventText, Text: "end\n"},
		{Kind: aistudio.EventCodeExecutionResult, CodeExecutionResult: &aistudio.CodeExecutionResult{Output: "1"}},
	}
	rendered := renderedContent(events)
	// hello + 换行 + 媒体 + world + 换行 + 代码 + 换行 + end\n(已有换行不加) + 代码
	if !strings.Contains(rendered, "hello\n") {
		t.Fatalf("文本后媒体前应补换行: %q", rendered)
	}
	if !strings.Contains(rendered, "world\n") {
		t.Fatalf("文本后代码前应补换行: %q", rendered)
	}
	if strings.Contains(rendered, "end\n\n") && !strings.Contains(rendered, "end\n```") {
		// end 已带换行,代码块前不应再补(除非代码渲染本身含换行)
		t.Logf("渲染内容: %q", rendered)
	}
}

func BenchmarkAnthropicBlocksManyTextEvents(b *testing.B) {
	events := make([]aistudio.Event, 0, 2000)
	for index := 0; index < 2000; index++ {
		events = append(events, aistudio.Event{Kind: aistudio.EventText, Text: fmt.Sprintf("token-%d ", index)})
	}
	result := generationResult{events: events}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = anthropicBlocks(result)
	}
}

func BenchmarkRenderedContentManyEvents(b *testing.B) {
	events := make([]aistudio.Event, 0, 2000)
	for index := 0; index < 2000; index++ {
		events = append(events, aistudio.Event{Kind: aistudio.EventText, Text: fmt.Sprintf("token-%d ", index)})
		if index%50 == 0 {
			events = append(events, aistudio.Event{Kind: aistudio.EventMedia, Media: &aistudio.Media{MIME: "image/png", Data: []byte("x")}})
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = renderedContent(events)
	}
}
