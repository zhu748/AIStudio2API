package api

import "encoding/json"

// 本文件集中定义三套协议的出站 wire 结构体(M2)。
//
// 此前流式热路径用 map[string]any 构建每帧响应:encoding/json 对 map 需要
// 按字典序排序键、并经 any 接口逐值反射,长对话每秒上百事件时 CPU 与分配
// 都被序列化吃掉。改为带 json tag 的 struct 后走缓存的字段编码路径,
// 分配从"每帧多个 map + 键值装箱"降为"每帧一个定长结构体"。
//
// 关键兼容性约束:每个字段的键存在性必须与旧 map 逐键等价——
//   - map 中无条件写入的键 → struct 字段不带 omitempty(零值照常输出)
//   - map 中条件写入的键   → struct 字段带 omitempty,或用指针类型
//     精确保留 "空字符串/nil 切片也输出" 的旧语义
// 字段顺序与旧 map 的字典序不同,但 JSON 对象键序无语义,客户端按键解析。

// ============================== Gemini ==============================

// geminiCountTokensOut 对应 countTokens 响应 {"totalTokens":N}
type geminiCountTokensOut struct {
	TotalTokens int64 `json:"totalTokens"`
}

// geminiModelsListOut 对应 GET /v1beta/models 响应
type geminiModelsListOut struct {
	Models []geminiModelOut `json:"models"`
}

// geminiModelOut 对应单个模型对象;6 个基础键无条件输出,
// capabilities/capabilityOptions/accessModes/paid 仅在非空/为真时输出。
type geminiModelOut struct {
	Name                       string              `json:"name"`
	DisplayName                string              `json:"displayName"`
	Description                string              `json:"description"`
	SupportedGenerationMethods []string            `json:"supportedGenerationMethods"`
	InputTokenLimit            int64               `json:"inputTokenLimit"`
	OutputTokenLimit           int64               `json:"outputTokenLimit"`
	Capabilities               map[string]bool     `json:"capabilities,omitempty"`
	CapabilityOptions          map[string][]string `json:"capabilityOptions,omitempty"`
	AccessModes                []int64             `json:"accessModes,omitempty"`
	Paid                       bool                `json:"paid,omitempty"`
}

// geminiGenerateResponseOut 对应非流式 generateContent 响应
type geminiGenerateResponseOut struct {
	Candidates    []geminiCandidateOut    `json:"candidates"`
	ModelVersion  string                  `json:"modelVersion"`
	ResponseID    string                  `json:"responseId"`
	UsageMetadata *geminiUsageMetadataOut `json:"usageMetadata,omitempty"`
}

// geminiStreamChunkOut 对应流式 generateContent 每帧(被写出的帧必带 candidates);
// 收尾帧额外携带 usageMetadata
type geminiStreamChunkOut struct {
	ResponseID    string                  `json:"responseId"`
	ModelVersion  string                  `json:"modelVersion"`
	Candidates    []geminiCandidateOut    `json:"candidates"`
	UsageMetadata *geminiUsageMetadataOut `json:"usageMetadata,omitempty"`
}

// geminiCandidateOut 覆盖三种形态:
//   - 非流式:content+index+finishReason(+message/grounding/citation)
//   - 流式内容帧:content+index
//   - 流式 grounding/citation 帧:grounding/citation+index
//   - 流式收尾帧:index+finishReason(+message)
type geminiCandidateOut struct {
	Content           *geminiContentOut       `json:"content,omitempty"`
	Index             int                     `json:"index"`
	FinishReason      string                  `json:"finishReason,omitempty"`
	FinishMessage     string                  `json:"finishMessage,omitempty"`
	GroundingMetadata *geminiGroundingMetaOut `json:"groundingMetadata,omitempty"`
	CitationMetadata  *geminiCitationMetaOut  `json:"citationMetadata,omitempty"`
}

type geminiContentOut struct {
	Role  string          `json:"role"`
	Parts []geminiPartOut `json:"parts"`
}

// geminiPartOut 统一承载 text/thought/签名/函数调用/代码执行/媒体等
// 所有 part 形态;Text 用指针以保留"text":""仍输出的旧语义。
type geminiPartOut struct {
	Text                  *string                       `json:"text,omitempty"`
	Thought               bool                          `json:"thought,omitempty"`
	ThoughtSignature      string                        `json:"thoughtSignature,omitempty"`
	InlineData            *geminiInlineDataOut          `json:"inlineData,omitempty"`
	FileData              *geminiFileDataOut            `json:"fileData,omitempty"`
	FunctionCall          *geminiFunctionCallOut        `json:"functionCall,omitempty"`
	ExecutableCode        *geminiExecutableCodeOut      `json:"executableCode,omitempty"`
	CodeExecutionResult   *geminiCodeExecutionResultOut `json:"codeExecutionResult,omitempty"`
	TranscriptionMetadata *geminiTranscriptionMetaOut   `json:"transcriptionMetadata,omitempty"`
}

type geminiInlineDataOut struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiFileDataOut struct {
	MIMEType    string `json:"mimeType"`
	FileURI     string `json:"fileUri"`
	DisplayName string `json:"displayName"`
}

type geminiFunctionCallOut struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type geminiExecutableCodeOut struct {
	Language string `json:"language"`
	Code     string `json:"code"`
}

// geminiCodeExecutionResultOut:output 与 error 互斥,outcome 决定哪个键出现
type geminiCodeExecutionResultOut struct {
	Outcome string  `json:"outcome"`
	Output  *string `json:"output,omitempty"`
	Error   *string `json:"error,omitempty"`
}

type geminiTranscriptionMetaOut struct {
	Speaker    string                     `json:"speaker,omitempty"`
	Timestamps []geminiTranscriptRangeOut `json:"timestamps,omitempty"`
}

type geminiTranscriptRangeOut struct {
	Start geminiTranscriptDurationOut `json:"start"`
	End   geminiTranscriptDurationOut `json:"end"`
}

type geminiTranscriptDurationOut struct {
	Seconds int64 `json:"seconds"`
	Nanos   int64 `json:"nanos"`
}

type geminiCitationMetaOut struct {
	CitationSources []geminiCitationSourceOut `json:"citationSources"`
}

type geminiCitationSourceOut struct {
	URI        string `json:"uri"`
	Title      string `json:"title"`
	StartIndex int    `json:"startIndex"`
	EndIndex   int    `json:"endIndex"`
}

// geminiGroundingMetaOut 各键沿用旧 map 的条件写入语义;
// searchEntryPoint 指针在非 nil 时输出(即使内部两字段皆空,输出 {})
type geminiGroundingMetaOut struct {
	SearchEntryPoint             *geminiSearchEntryOut       `json:"searchEntryPoint,omitempty"`
	GroundingChunks              []geminiGroundingChunkOut   `json:"groundingChunks,omitempty"`
	GroundingSupports            []geminiGroundingSupportOut `json:"groundingSupports,omitempty"`
	RetrievalMetadata            *geminiRetrievalMetaOut     `json:"retrievalMetadata,omitempty"`
	WebSearchQueries             []string                    `json:"webSearchQueries,omitempty"`
	GoogleMapsWidgetContextToken string                      `json:"googleMapsWidgetContextToken,omitempty"`
}

type geminiSearchEntryOut struct {
	RenderedContent string `json:"renderedContent,omitempty"`
	SDKBlob         string `json:"sdkBlob,omitempty"`
}

// geminiGroundingChunkOut 三种来源形态互斥:web 不带 text,
// retrievedContext 带 text,maps 带 text+placeId(键均无条件输出)
type geminiGroundingChunkOut struct {
	Web              *geminiChunkWebOut       `json:"web,omitempty"`
	RetrievedContext *geminiChunkRetrievedOut `json:"retrievedContext,omitempty"`
	Maps             *geminiChunkMapsOut      `json:"maps,omitempty"`
}

type geminiChunkWebOut struct {
	URI   string `json:"uri"`
	Title string `json:"title"`
}

type geminiChunkRetrievedOut struct {
	URI   string `json:"uri"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

type geminiChunkMapsOut struct {
	URI     string `json:"uri"`
	Title   string `json:"title"`
	Text    string `json:"text"`
	PlaceID string `json:"placeId"`
}

type geminiGroundingSupportOut struct {
	Segment               geminiSegmentOut `json:"segment"`
	GroundingChunkIndices []int            `json:"groundingChunkIndices"`
	ConfidenceScores      []float64        `json:"confidenceScores,omitempty"`
}

type geminiSegmentOut struct {
	PartIndex  int    `json:"partIndex"`
	StartIndex int    `json:"startIndex"`
	EndIndex   int    `json:"endIndex"`
	Text       string `json:"text"`
}

type geminiRetrievalMetaOut struct {
	GoogleSearchDynamicRetrievalScore float64 `json:"googleSearchDynamicRetrievalScore"`
}

// geminiUsageMetadataOut 五键无条件输出(与旧 map 一致,零值也输出)
type geminiUsageMetadataOut struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
}

// geminiStreamErrorOut 对应流式错误帧;非流式错误沿用 errors.go 的 writeGeminiError
type geminiStreamErrorOut struct {
	Error geminiErrorInfoOut `json:"error"`
}

type geminiErrorInfoOut struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// ============================== Anthropic ==============================

// anthropicMessageResponseOut 对应非流式 /v1/messages 响应
type anthropicMessageResponseOut struct {
	ID                   string                  `json:"id"`
	Type                 string                  `json:"type"`
	Role                 string                  `json:"role"`
	Model                string                  `json:"model"`
	Content              []anthropicContentBlock `json:"content"`
	StopReason           string                  `json:"stop_reason"`
	StopSequence         *string                 `json:"stop_sequence"`
	ProviderModel        string                  `json:"provider_model,omitempty"`
	ProviderFinishReason string                  `json:"provider_finish_reason,omitempty"`
	Usage                *anthropicUsageBody     `json:"usage,omitempty"`
}

// anthropicUsageBody 对应两键无条件输出的 usage
type anthropicUsageBody struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// anthropicUsageDeltaOut 对应 message_delta 中的 usage:
// 旧实现仅在 usage 非 nil 时写 input_tokens(此时即使 0 也输出),
// output_tokens 无条件输出(默认 0)
type anthropicUsageDeltaOut struct {
	InputTokens  *int64 `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens"`
}

type anthropicModelsListOut struct {
	Data    []anthropicModelItemOut `json:"data"`
	HasMore bool                    `json:"has_more"`
	FirstID *string                 `json:"first_id"`
	LastID  *string                 `json:"last_id"`
}

type anthropicModelItemOut struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

// ---- Anthropic 流式事件 ----

type anthropicMessageStartEvent struct {
	Type    string                 `json:"type"`
	Message anthropicStreamMessage `json:"message"`
}

// anthropicStreamMessage 对应 message_start 中的 message 对象:
// content 输出 []、stop_reason/stop_sequence 输出 null
type anthropicStreamMessage struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Model        string                  `json:"model"`
	Content      []anthropicContentBlock `json:"content"`
	StopReason   *string                 `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        anthropicUsageBody      `json:"usage"`
}

type anthropicBlockDeltaEvent struct {
	Type  string            `json:"type"`
	Index int               `json:"index"`
	Delta anthropicDeltaOut `json:"delta"`
}

// anthropicDeltaOut 四种 delta 形态共用;文本类用指针保住 "空串也输出" 语义
type anthropicDeltaOut struct {
	Type        string  `json:"type"`
	Text        *string `json:"text,omitempty"`
	Thinking    *string `json:"thinking,omitempty"`
	PartialJSON *string `json:"partial_json,omitempty"`
	Signature   *string `json:"signature,omitempty"`
}

type anthropicBlockStartEvent struct {
	Type         string            `json:"type"`
	Index        int               `json:"index"`
	ContentBlock anthropicBlockOut `json:"content_block"`
}

// anthropicBlockOut 覆盖 text/thinking/tool_use/redacted_thinking 四种起始块
type anthropicBlockOut struct {
	Type     string          `json:"type"`
	Text     *string         `json:"text,omitempty"`
	Thinking *string         `json:"thinking,omitempty"`
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Data     string          `json:"data,omitempty"`
}

type anthropicBlockStopEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type anthropicMessageDeltaEvent struct {
	Type  string                    `json:"type"`
	Delta anthropicMessageDeltaBody `json:"delta"`
	Usage anthropicUsageDeltaOut    `json:"usage"`
}

type anthropicMessageDeltaBody struct {
	StopReason           string  `json:"stop_reason"`
	StopSequence         *string `json:"stop_sequence"`
	ProviderFinishReason string  `json:"provider_finish_reason,omitempty"`
}

type anthropicMessageStopEvent struct {
	Type string `json:"type"`
}

type anthropicErrorEventOut struct {
	Type  string               `json:"type"`
	Error anthropicErrorDetail `json:"error"`
}

type anthropicErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ============================== OpenAI ==============================

// openAICompletionOut 对应非流式 chat completion
type openAICompletionOut struct {
	ID            string                   `json:"id"`
	Object        string                   `json:"object"`
	Created       int64                    `json:"created"`
	Model         string                   `json:"model"`
	Choices       []openAIChoiceMessageOut `json:"choices"`
	ProviderModel string                   `json:"provider_model,omitempty"`
	Usage         *openAIUsageOut          `json:"usage,omitempty"`
}

type openAIChoiceMessageOut struct {
	Index                int              `json:"index"`
	Message              openAIMessageOut `json:"message"`
	FinishReason         string           `json:"finish_reason"`
	ProviderFinishReason string           `json:"provider_finish_reason,omitempty"`
}

// openAIMessageOut content 用指针:工具调用且无正文时输出 null(与旧 map 一致)
type openAIMessageOut struct {
	Role             string                `json:"role"`
	Content          *string               `json:"content"`
	ReasoningContent string                `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIToolCallOut   `json:"tool_calls,omitempty"`
	Annotations      []openAIAnnotationOut `json:"annotations,omitempty"`
}

type openAIToolCallOut struct {
	ID           string                   `json:"id"`
	Type         string                   `json:"type"`
	Function     openAIToolFunctionOut    `json:"function"`
	ExtraContent *openAISignatureExtraOut `json:"extra_content,omitempty"`
}

// openAIChunkOut 对应流式 chat completion 每帧。
// Usage 为 any:includeUsage 时写入 typed-nil 指针,既不被 omitempty
// 折叠(接口非 nil)又序列化为 null——精确复刻旧 map 的 "usage":null。
type openAIChunkOut struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []openAIChoiceChunkOut `json:"choices"`
	Usage   any                    `json:"usage,omitempty"`
}

type openAIChoiceChunkOut struct {
	Index                int            `json:"index"`
	Delta                openAIDeltaOut `json:"delta"`
	FinishReason         *string        `json:"finish_reason"`
	ProviderFinishReason string         `json:"provider_finish_reason,omitempty"`
}

// openAIDeltaOut 五种形态共用;role 首帧恒为 "assistant"。
// 首帧 {"role":"assistant","content":""} 的空串 content 用指针保留。
type openAIDeltaOut struct {
	Role             string                   `json:"role,omitempty"`
	Content          *string                  `json:"content,omitempty"`
	ReasoningContent *string                  `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIToolCallChunkOut `json:"tool_calls,omitempty"`
	Annotations      []openAIAnnotationOut    `json:"annotations,omitempty"`
}

// openAIToolCallChunkOut 流式 delta 中的工具调用(带 index)
type openAIToolCallChunkOut struct {
	Index        int                      `json:"index"`
	ID           string                   `json:"id"`
	Type         string                   `json:"type"`
	Function     openAIToolFunctionOut    `json:"function"`
	ExtraContent *openAISignatureExtraOut `json:"extra_content,omitempty"`
}

type openAIToolFunctionOut struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAISignatureExtraOut struct {
	Google openAIGoogleSignatureOut `json:"google"`
}

type openAIGoogleSignatureOut struct {
	ThoughtSignature string `json:"thought_signature"`
}

// openAIUsageChunkOut 对应 include_usage 收尾帧(choices 为空数组)
type openAIUsageChunkOut struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []openAIChoiceChunkOut `json:"choices"`
	Usage   openAIUsageOut         `json:"usage"`
}

type openAIAnnotationOut struct {
	Type        string               `json:"type"`
	URLCitation openAIURLCitationOut `json:"url_citation"`
}

type openAIURLCitationOut struct {
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
	Title      string `json:"title"`
	URL        string `json:"url"`
}

// openAIUsageOut 四键无条件输出,内层 reasoning_tokens 同样无条件
type openAIUsageOut struct {
	PromptTokens            int64                     `json:"prompt_tokens"`
	CompletionTokens        int64                     `json:"completion_tokens"`
	TotalTokens             int64                     `json:"total_tokens"`
	CompletionTokensDetails openAICompletionDetailOut `json:"completion_tokens_details"`
}

type openAICompletionDetailOut struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// openAIStreamErrorOut 对应流式错误帧
type openAIStreamErrorOut struct {
	Error openAIErrorBodyOut `json:"error"`
}

type openAIErrorBodyOut struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// openAIModelsListOut 对应 GET /v1/models 响应
type openAIModelsListOut struct {
	Object string           `json:"object"`
	Data   []openAIModelOut `json:"data"`
}

type openAIModelOut struct {
	ID                         string              `json:"id"`
	Object                     string              `json:"object"`
	Created                    int64               `json:"created"`
	OwnedBy                    string              `json:"owned_by"`
	Name                       string              `json:"name"`
	Description                string              `json:"description"`
	SupportedGenerationMethods []string            `json:"supported_generation_methods"`
	InputTokenLimit            int64               `json:"input_token_limit"`
	OutputTokenLimit           int64               `json:"output_token_limit"`
	Capabilities               map[string]bool     `json:"capabilities,omitempty"`
	CapabilityOptions          map[string][]string `json:"capability_options,omitempty"`
	AccessModes                []int64             `json:"access_modes,omitempty"`
	Paid                       bool                `json:"paid,omitempty"`
}

// ====================== OpenAI Responses(热帧) ======================

// responsesTextDeltaOut 对应 response.output_text.delta 每帧(每 token 触发)
type responsesTextDeltaOut struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   int    `json:"content_index"`
	Delta          string `json:"delta"`
	Logprobs       []any  `json:"logprobs"`
}

// responsesReasoningDeltaOut 对应 response.reasoning_summary_text.delta 每帧
type responsesReasoningDeltaOut struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	SummaryIndex   int    `json:"summary_index"`
	Delta          string `json:"delta"`
}

// responsesShellEventOut 对应 response.created / response.in_progress
// 每请求双帧:负载为完整 response shell(含 tools 大 schema),
// 两帧内容一致仅序号不同,调用方复用同一 shell 只推进 sequence。
type responsesShellEventOut struct {
	Type           string         `json:"type"`
	SequenceNumber int            `json:"sequence_number"`
	Response       map[string]any `json:"response"`
}

// responsesFunctionCallDeltaOut 对应 response.function_call_arguments.delta
// 帧:arguments 为完整工具调用 JSON,可能为大 payload 帧。
type responsesFunctionCallDeltaOut struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	Delta          string `json:"delta"`
}

// responsesFunctionCallDoneOut 对应 response.function_call_arguments.done 帧
type responsesFunctionCallDoneOut struct {
	Type           string `json:"type"`
	SequenceNumber int    `json:"sequence_number"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	Arguments      string `json:"arguments"`
	Name           string `json:"name"`
}
