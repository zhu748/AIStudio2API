package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type chatRequest struct {
	Model               string            `json:"model"`
	Messages            []chatMessage     `json:"messages"`
	Stream              bool              `json:"stream"`
	StreamOptions       chatStreamOptions `json:"stream_options"`
	Tools               []openAITool      `json:"tools"`
	ToolChoice          json.RawMessage   `json:"tool_choice"`
	Temperature         *float64          `json:"temperature"`
	TopP                *float64          `json:"top_p"`
	MaxTokens           *int64            `json:"max_tokens"`
	MaxCompletionTokens *int64            `json:"max_completion_tokens"`
	FrequencyPenalty    *float64          `json:"frequency_penalty"`
	PresencePenalty     *float64          `json:"presence_penalty"`
	N                   *int64            `json:"n"`
	ParallelToolCalls   *bool             `json:"parallel_tool_calls"`
	Logprobs            *bool             `json:"logprobs"`
	LogitBias           json.RawMessage   `json:"logit_bias"`
	Stop                json.RawMessage   `json:"stop"`
	ResponseFormat      json.RawMessage   `json:"response_format"`
	ReasoningEffort     string            `json:"reasoning_effort"`
	Reasoning           json.RawMessage   `json:"reasoning"`
	Seed                *int64            `json:"seed"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"`
	Name       string           `json:"name"`
	ToolCallID string           `json:"tool_call_id"`
	ToolCalls  []openAIToolCall `json:"tool_calls"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	ExtraContent struct {
		Google struct {
			ThoughtSignature string `json:"thought_signature"`
		} `json:"google"`
	} `json:"extra_content"`
}

type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      *bool           `json:"strict"`
	} `json:"function"`
}

var assistantImagePattern = regexp.MustCompile(`!\[[^\]]*\]\((data:image/[A-Za-z0-9.+-]+;base64,[A-Za-z0-9+/=]+)\)`)

func (s *server) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.service.Models(r.Context())
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeOpenAIError(w, statusFromError(err), openAIErrorCode(err), err.Error())
		}
		return
	}
	if r.Header.Get("Anthropic-Version") != "" {
		writeAnthropicModels(w, models)
		return
	}
	data := make([]openAIModelOut, 0, len(models))
	for _, model := range models {
		data = append(data, openAIModelOut{
			ID:                         model.ID,
			Object:                     "model",
			Created:                    0,
			OwnedBy:                    "google",
			Name:                       model.Name,
			Description:                model.Description,
			SupportedGenerationMethods: model.Methods,
			InputTokenLimit:            model.InputTokenLimit,
			OutputTokenLimit:           model.OutputTokenLimit,
			Capabilities:               model.Capabilities,
			CapabilityOptions:          model.CapabilityOptions,
			AccessModes:                model.AccessModes,
			Paid:                       model.Paid,
		})
	}
	writeJSON(w, http.StatusOK, openAIModelsListOut{Object: "list", Data: data})
}

func (s *server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var request chatRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if request.Model == "" || len(request.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model and messages are required")
		return
	}
	requestID := newID("chatcmpl")
	generateRequest, err := request.toGenerateRequest(requestID)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	events, err := s.service.Generate(r.Context(), generateRequest)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeOpenAIError(w, statusFromError(err), openAIErrorCode(err), err.Error())
		}
		return
	}
	created := time.Now().Unix()
	if request.Stream {
		s.streamChatCompletion(w, r, request, requestID, created, events)
		return
	}
	result, err := consumeEvents(r.Context(), events, nil)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeOpenAIError(w, statusFromError(err), openAIErrorCode(err), err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, buildChatCompletion(requestID, created, request.Model, result))
}

func (request chatRequest) toGenerateRequest(id string) (aistudio.GenerateRequest, error) {
	var system []string
	contents := make([]aistudio.Content, 0, len(request.Messages))
	for _, message := range request.Messages {
		if message.Role == "system" || message.Role == "developer" {
			text, err := openAITextContent(message.Content)
			if err != nil {
				return aistudio.GenerateRequest{}, fmt.Errorf("%s message: %w", message.Role, err)
			}
			if text != "" {
				system = append(system, text)
			}
			continue
		}
		content, err := chatMessageContent(message)
		if err != nil {
			return aistudio.GenerateRequest{}, err
		}
		contents = append(contents, content)
	}
	tools, err := mapOpenAITools(request.Tools, request.ToolChoice)
	if err != nil {
		return aistudio.GenerateRequest{}, err
	}
	config, err := request.generationConfig()
	if err != nil {
		return aistudio.GenerateRequest{}, err
	}
	return aistudio.GenerateRequest{
		ID:       id,
		Model:    request.Model,
		System:   strings.Join(system, "\n"),
		Contents: contents,
		Config:   config,
		Tools:    tools,
	}, nil
}

func chatMessageContent(message chatMessage) (aistudio.Content, error) {
	role, err := openAIRole(message.Role)
	if err != nil {
		return aistudio.Content{}, err
	}
	if role == aistudio.RoleTool {
		content, err := normalizeFunctionResultContent(message.Content)
		if err != nil {
			return aistudio.Content{}, fmt.Errorf("tool message content: %w", err)
		}
		return aistudio.Content{Role: role, Parts: []aistudio.Part{{FunctionResult: &aistudio.FunctionResult{
			ID:      message.ToolCallID,
			Name:    message.Name,
			Content: content,
		}}}}, nil
	}
	parts, err := openAIContentParts(message.Content)
	if err != nil {
		return aistudio.Content{}, fmt.Errorf("%s message content: %w", message.Role, err)
	}
	if role == aistudio.RoleAssistant {
		parts, err = openAIAssistantContentParts(message.Content, parts)
		if err != nil {
			return aistudio.Content{}, fmt.Errorf("assistant message content: %w", err)
		}
	}
	for _, call := range message.ToolCalls {
		if call.Type != "" && call.Type != "function" {
			return aistudio.Content{}, fmt.Errorf("unsupported tool call type %q", call.Type)
		}
		arguments := json.RawMessage(call.Function.Arguments)
		if len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		if !json.Valid(arguments) {
			return aistudio.Content{}, fmt.Errorf("tool call %q arguments must be JSON", call.Function.Name)
		}
		parts = append(parts, aistudio.Part{FunctionCall: &aistudio.FunctionCall{
			ID:               call.ID,
			Name:             call.Function.Name,
			Arguments:        arguments,
			ThoughtSignature: call.ExtraContent.Google.ThoughtSignature,
		}})
	}
	return aistudio.Content{Role: role, Parts: parts}, nil
}

func openAIAssistantContentParts(raw json.RawMessage, fallback []aistudio.Part) ([]aistudio.Part, error) {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return fallback, nil
	}
	matches := assistantImagePattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return fallback, nil
	}
	parts := make([]aistudio.Part, 0, len(matches)*2+1)
	position := 0
	for _, match := range matches {
		if match[0] > position {
			parts = append(parts, aistudio.Part{Text: text[position:match[0]]})
		}
		part, err := fileOrInlinePart(text[match[2]:match[3]], "")
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
		position = match[1]
	}
	if position < len(text) {
		parts = append(parts, aistudio.Part{Text: text[position:]})
	}
	return parts, nil
}

func openAIRole(role string) (aistudio.Role, error) {
	switch role {
	case "user":
		return aistudio.RoleUser, nil
	case "assistant":
		return aistudio.RoleAssistant, nil
	case "tool", "function":
		return aistudio.RoleTool, nil
	default:
		return "", fmt.Errorf("unsupported message role %q", role)
	}
}

func openAITextContent(raw json.RawMessage) (string, error) {
	parts, err := openAIContentParts(raw)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, part := range parts {
		if part.Text == "" && (part.InlineData != nil || part.File != nil) {
			return "", fmt.Errorf("system content must be text")
		}
		text.WriteString(part.Text)
	}
	return text.String(), nil
}

func openAIContentParts(raw json.RawMessage) ([]aistudio.Part, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []aistudio.Part{{Text: text}}, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("content must be a string or array")
	}
	parts := make([]aistudio.Part, 0, len(blocks))
	for _, block := range blocks {
		part, err := openAIContentPart(block)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func openAIContentPart(raw json.RawMessage) (aistudio.Part, error) {
	var block struct {
		Type       string          `json:"type"`
		Text       string          `json:"text"`
		ImageURL   json.RawMessage `json:"image_url"`
		VideoURL   json.RawMessage `json:"video_url"`
		FileID     string          `json:"file_id"`
		Filename   string          `json:"filename"`
		FileData   string          `json:"file_data"`
		InputAudio *struct {
			Data   string `json:"data"`
			Format string `json:"format"`
		} `json:"input_audio"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return aistudio.Part{}, err
	}
	switch block.Type {
	case "text", "input_text", "output_text":
		return aistudio.Part{Text: block.Text}, nil
	case "image_url", "input_image":
		url, err := imageURLString(block.ImageURL)
		if err != nil {
			return aistudio.Part{}, err
		}
		return fileOrInlinePart(url, "")
	case "video_url", "input_video":
		url, err := imageURLString(block.VideoURL)
		if err != nil {
			return aistudio.Part{}, err
		}
		media, ok := aistudio.ExternalMediaForURL(url)
		if !ok {
			return aistudio.Part{}, fmt.Errorf("video_url must contain a YouTube video URL")
		}
		return aistudio.Part{ExternalMedia: media}, nil
	case "file", "input_file":
		if block.FileData != "" {
			return fileOrInlinePart(block.FileData, "")
		}
		return aistudio.Part{File: &aistudio.FileRef{ID: block.FileID, Name: block.Filename}}, nil
	case "input_audio":
		if block.InputAudio == nil {
			return aistudio.Part{}, fmt.Errorf("input_audio is required")
		}
		data, err := base64.StdEncoding.DecodeString(block.InputAudio.Data)
		if err != nil {
			return aistudio.Part{}, fmt.Errorf("input_audio.data: %w", err)
		}
		return aistudio.Part{InlineData: &aistudio.Blob{MIME: audioMIME(block.InputAudio.Format), Data: data}}, nil
	default:
		return aistudio.Part{}, fmt.Errorf("unsupported content type %q", block.Type)
	}
}

func imageURLString(raw json.RawMessage) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value, nil
	}
	var object struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &object); err != nil || object.URL == "" {
		return "", fmt.Errorf("image_url must contain url")
	}
	return object.URL, nil
}

func fileOrInlinePart(value string, name string) (aistudio.Part, error) {
	if media, ok := aistudio.ExternalMediaForURL(value); ok {
		return aistudio.Part{ExternalMedia: media}, nil
	}
	if !strings.HasPrefix(value, "data:") {
		return aistudio.Part{File: &aistudio.FileRef{ID: value, Name: name}}, nil
	}
	metadata, encoded, ok := strings.Cut(strings.TrimPrefix(value, "data:"), ",")
	if !ok || !strings.HasSuffix(metadata, ";base64") {
		return aistudio.Part{}, fmt.Errorf("data URL must use base64")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return aistudio.Part{}, fmt.Errorf("data URL: %w", err)
	}
	return aistudio.Part{InlineData: &aistudio.Blob{MIME: strings.TrimSuffix(metadata, ";base64"), Data: data}}, nil
}

func audioMIME(format string) string {
	switch strings.ToLower(format) {
	case "mp3", "mpeg":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	default:
		return "audio/" + strings.ToLower(format)
	}
}

func mapOpenAITools(tools []openAITool, choice json.RawMessage) (aistudio.Tools, error) {
	var mapped aistudio.Tools
	for _, tool := range tools {
		switch tool.Type {
		case "function":
			if tool.Function.Name == "" {
				return aistudio.Tools{}, fmt.Errorf("function tool name is required")
			}
			if tool.Function.Strict != nil && *tool.Function.Strict {
				return aistudio.Tools{}, fmt.Errorf("function tool strict is not supported by AI Studio Web")
			}
			parameters := tool.Function.Parameters
			if len(parameters) == 0 {
				parameters = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			mapped.Functions = append(mapped.Functions, aistudio.FunctionDeclaration{
				Name:        tool.Function.Name,
				Description: tool.Function.Description,
				Parameters:  parameters,
			})
		case "web_search", "web_search_preview":
			mapped.Google = appendUnique(mapped.Google, "google_search")
		case "code_interpreter":
			mapped.Google = appendUnique(mapped.Google, "code_execution")
		case "url_context":
			mapped.Google = appendUnique(mapped.Google, "url_context")
		case "google_maps":
			mapped.Google = appendUnique(mapped.Google, "google_maps")
		case "image_search":
			mapped.Google = appendUnique(mapped.Google, "image_search")
		default:
			return aistudio.Tools{}, fmt.Errorf("unsupported tool type %q", tool.Type)
		}
	}
	config, err := openAIToolChoice(choice)
	if err != nil {
		return aistudio.Tools{}, err
	}
	if len(mapped.Functions) == 0 && len(mapped.Google) == 0 {
		return mapped, nil
	}
	mapped.ToolConfig = config
	return mapped, nil
}

func openAIToolChoice(raw json.RawMessage) (aistudio.ToolConfig, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return aistudio.ToolConfig{Mode: "auto"}, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch mode {
		case "auto", "none":
			return aistudio.ToolConfig{Mode: mode}, nil
		case "required":
			return aistudio.ToolConfig{}, fmt.Errorf("tool_choice required is not supported by AI Studio Web")
		default:
			return aistudio.ToolConfig{}, fmt.Errorf("unsupported tool_choice %q", mode)
		}
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return aistudio.ToolConfig{}, fmt.Errorf("invalid tool_choice: %w", err)
	}
	if object == nil {
		return aistudio.ToolConfig{}, fmt.Errorf("invalid tool_choice")
	}
	return aistudio.ToolConfig{}, fmt.Errorf("named tool_choice is not supported by AI Studio Web")
}

func appendUnique(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func (request chatRequest) generationConfig() (aistudio.GenerationConfig, error) {
	if request.N != nil && *request.N != 1 {
		return aistudio.GenerationConfig{}, fmt.Errorf("n must be 1")
	}
	if request.ParallelToolCalls != nil && !*request.ParallelToolCalls {
		return aistudio.GenerationConfig{}, fmt.Errorf("parallel_tool_calls must be true")
	}
	if request.Logprobs != nil && *request.Logprobs {
		return aistudio.GenerationConfig{}, fmt.Errorf("logprobs must be false")
	}
	if rawJSONConfigured(request.LogitBias) {
		var biases map[string]json.RawMessage
		if err := json.Unmarshal(request.LogitBias, &biases); err != nil || biases == nil {
			return aistudio.GenerationConfig{}, fmt.Errorf("logit_bias must be an empty object")
		}
		if len(biases) != 0 {
			return aistudio.GenerationConfig{}, fmt.Errorf("logit_bias must be empty")
		}
	}
	if request.FrequencyPenalty != nil && *request.FrequencyPenalty != 0 {
		return aistudio.GenerationConfig{}, fmt.Errorf("frequency_penalty must be 0")
	}
	if request.PresencePenalty != nil && *request.PresencePenalty != 0 {
		return aistudio.GenerationConfig{}, fmt.Errorf("presence_penalty must be 0")
	}
	config := aistudio.GenerationConfig{
		Temperature:     request.Temperature,
		TopP:            request.TopP,
		ReasoningEffort: request.ReasoningEffort,
		Seed:            request.Seed,
	}
	if request.MaxCompletionTokens != nil {
		config.MaxOutputTokens = request.MaxCompletionTokens
	} else {
		config.MaxOutputTokens = request.MaxTokens
	}
	stop, err := decodeStopSequences(request.Stop)
	if err != nil {
		return config, err
	}
	config.StopSequences = stop
	if len(request.Reasoning) > 0 && string(request.Reasoning) != "null" {
		var reasoning struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(request.Reasoning, &reasoning); err != nil {
			return config, fmt.Errorf("invalid reasoning: %w", err)
		}
		if reasoning.Effort != "" {
			config.ReasoningEffort = reasoning.Effort
		}
	}
	if len(request.ResponseFormat) > 0 && string(request.ResponseFormat) != "null" {
		var format struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"json_schema"`
		}
		if err := json.Unmarshal(request.ResponseFormat, &format); err != nil {
			return config, fmt.Errorf("invalid response_format: %w", err)
		}
		switch format.Type {
		case "json_object":
			config.ResponseMIMEType = "application/json"
		case "json_schema":
			config.ResponseMIMEType = "application/json"
			config.ResponseSchema = format.JSONSchema.Schema
		case "", "text":
		default:
			return config, fmt.Errorf("unsupported response_format type %q", format.Type)
		}
	}
	return config, nil
}

func decodeStopSequences(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return normalizeStopSequences([]string{single}), nil
	}
	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err != nil {
		return nil, fmt.Errorf("stop must be a string or string array")
	}
	return normalizeStopSequences(multiple), nil
}

// normalizeStopSequences 删除不会形成停止条件的空字符串
func normalizeStopSequences(values []string) []string {
	var normalized []string
	for _, value := range values {
		if value != "" {
			normalized = append(normalized, value)
		}
	}
	return normalized
}

func buildChatCompletion(id string, created int64, model string, result generationResult) openAICompletionOut {
	rendered := renderedContent(result.events)
	message := openAIMessageOut{Role: "assistant"}
	if rendered != "" || len(result.toolCalls) == 0 {
		message.Content = &rendered
	}
	if result.reasoning.Len() > 0 {
		message.ReasoningContent = result.reasoning.String()
	}
	if len(result.toolCalls) > 0 {
		message.ToolCalls = openAIToolCallOutput(result.toolCalls)
	}
	if len(result.citations) > 0 {
		message.Annotations = openAICitations(result.citations)
	}
	response := openAICompletionOut{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []openAIChoiceMessageOut{{
			Index:        0,
			Message:      message,
			FinishReason: openAIFinishReason(result.finishReason, len(result.toolCalls) > 0),
		}},
	}
	if providerReason := providerFinishReason(result.finishReason); providerReason != "" {
		response.Choices[0].ProviderFinishReason = providerReason
	}
	if result.providerModel != "" {
		response.ProviderModel = result.providerModel
	}
	if result.usage != nil {
		response.Usage = &openAIUsageOut{
			PromptTokens:     inputTokens(result.usage),
			CompletionTokens: outputTokens(result.usage),
			TotalTokens:      result.usage.TotalTokens,
			CompletionTokensDetails: openAICompletionDetailOut{
				ReasoningTokens: result.usage.ReasoningTokens,
			},
		}
	}
	return response
}

func (s *server) streamChatCompletion(w http.ResponseWriter, r *http.Request, request chatRequest, id string, created int64, events <-chan aistudio.Event) {
	streamHeaders(w)
	roleDelta := openAIDeltaOut{Role: "assistant"}
	emptyContent := ""
	roleDelta.Content = &emptyContent
	if err := writeChatChunk(w, id, created, request.Model, roleDelta, nil, request.StreamOptions.IncludeUsage, ""); err != nil {
		return
	}
	toolIndex := 0
	hasContent := false
	contentEndsWithNewline := false
	includeUsage := request.StreamOptions.IncludeUsage
	result, err := consumeStreamEvents(r.Context(), events, func(event aistudio.Event) error {
		switch event.Kind {
		case aistudio.EventText:
			hasContent = hasContent || event.Text != ""
			contentEndsWithNewline = strings.HasSuffix(event.Text, "\n")
			text := event.Text
			return writeChatChunk(w, id, created, request.Model, openAIDeltaOut{Content: &text}, nil, includeUsage, "")
		case aistudio.EventReasoning:
			reasoning := event.Text
			return writeChatChunk(w, id, created, request.Model, openAIDeltaOut{ReasoningContent: &reasoning}, nil, includeUsage, "")
		case aistudio.EventToolCall:
			if event.ToolCall == nil {
				return nil
			}
			call := event.ToolCall
			toolCall := openAIToolCallChunkOut{
				Index:    toolIndex,
				ID:       call.ID,
				Type:     "function",
				Function: openAIToolFunctionOut{Name: call.Name, Arguments: string(call.Arguments)},
			}
			if call.ThoughtSignature != "" {
				toolCall.ExtraContent = openAIGoogleThoughtSignature(call.ThoughtSignature)
			}
			delta := openAIDeltaOut{ToolCalls: []openAIToolCallChunkOut{toolCall}}
			toolIndex++
			return writeChatChunk(w, id, created, request.Model, delta, nil, includeUsage, "")
		case aistudio.EventMedia:
			if event.Media == nil {
				return nil
			}
			content := renderMediaMarkdown(*event.Media)
			if hasContent && !contentEndsWithNewline {
				content = "\n" + content
			}
			hasContent = true
			contentEndsWithNewline = false
			return writeChatChunk(w, id, created, request.Model, openAIDeltaOut{Content: &content}, nil, includeUsage, "")
		case aistudio.EventExecutableCode, aistudio.EventCodeExecutionResult:
			content := renderCodeExecution(event)
			if content == "" {
				return nil
			}
			if hasContent && !contentEndsWithNewline {
				content = "\n" + content
			}
			content += "\n"
			hasContent = true
			contentEndsWithNewline = true
			return writeChatChunk(w, id, created, request.Model, openAIDeltaOut{Content: &content}, nil, includeUsage, "")
		}
		return nil
	}, func() error { return writeSSEHeartbeat(w) })
	if err != nil {
		if shouldWriteRequestError(r, err) {
			status := statusFromError(err)
			code := openAIErrorCode(err)
			_ = writeSSE(w, "", openAIStreamErrorOut{Error: openAIErrorBodyOut{
				Message: err.Error(), Type: openAIErrorType(status, code), Code: code,
			}})
		}
		return
	}
	if len(result.citations) > 0 {
		_ = writeChatChunk(w, id, created, request.Model, openAIDeltaOut{Annotations: openAICitations(result.citations)}, nil, includeUsage, "")
	}
	finish := openAIFinishReason(result.finishReason, len(result.toolCalls) > 0)
	providerReason := providerFinishReason(result.finishReason)
	if err := writeChatChunk(w, id, created, request.Model, openAIDeltaOut{}, &finish, includeUsage, providerReason); err != nil {
		return
	}
	if includeUsage && result.usage != nil {
		chunk := openAIUsageChunkOut{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   request.Model,
			Choices: []openAIChoiceChunkOut{},
			Usage:   openAIUsage(result.usage),
		}
		if err := writeSSE(w, "", chunk); err != nil {
			return
		}
	}
	_ = writeSSEText(w, "[DONE]")
}

func writeChatChunk(w http.ResponseWriter, id string, created int64, model string, delta openAIDeltaOut, finish *string, includeUsage bool, providerReason string) error {
	chunk := openAIChunkOut{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []openAIChoiceChunkOut{{
			Index:                0,
			Delta:                delta,
			FinishReason:         finish,
			ProviderFinishReason: providerReason,
		}},
	}
	if includeUsage {
		// typed-nil 接口:不被 omitempty 折叠,序列化为 "usage":null
		chunk.Usage = (*openAIUsageOut)(nil)
	}
	return writeSSE(w, "", chunk)
}

func openAIFinishReason(reason string, hasTools bool) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "max_tokens", "max_output_tokens", "length":
		return "length"
	case "stop_sequence":
		return "stop"
	case "", "stop":
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	default:
		return "content_filter"
	}
}

func openAIToolCallOutput(calls []aistudio.FunctionCall) []openAIToolCallOut {
	output := make([]openAIToolCallOut, 0, len(calls))
	for _, call := range calls {
		item := openAIToolCallOut{
			ID:       call.ID,
			Type:     "function",
			Function: openAIToolFunctionOut{Name: call.Name, Arguments: string(call.Arguments)},
		}
		if call.ThoughtSignature != "" {
			item.ExtraContent = openAIGoogleThoughtSignature(call.ThoughtSignature)
		}
		output = append(output, item)
	}
	return output
}

func openAIGoogleThoughtSignature(signature string) *openAISignatureExtraOut {
	return &openAISignatureExtraOut{Google: openAIGoogleSignatureOut{ThoughtSignature: signature}}
}

func openAICitations(citations []aistudio.Citation) []openAIAnnotationOut {
	output := make([]openAIAnnotationOut, 0, len(citations))
	for _, citation := range citations {
		output = append(output, openAIAnnotationOut{
			Type: "url_citation",
			URLCitation: openAIURLCitationOut{
				StartIndex: citation.Start,
				EndIndex:   citation.End,
				Title:      citation.Title,
				URL:        citation.URL,
			},
		})
	}
	return output
}

func openAIUsage(usage *aistudio.Usage) openAIUsageOut {
	return openAIUsageOut{
		PromptTokens:     inputTokens(usage),
		CompletionTokens: outputTokens(usage),
		TotalTokens:      usage.TotalTokens,
		CompletionTokensDetails: openAICompletionDetailOut{
			ReasoningTokens: usage.ReasoningTokens,
		},
	}
}

func renderedContent(events []aistudio.Event) string {
	var content strings.Builder
	// 追踪末字节:此前用 strings.HasSuffix(content.String(), "\n") 判断,
	// 每次调用都会把整个累积内容拷贝成 string,长文本+多媒体交错时 O(n²)
	lastByte := byte(0)
	ensureNewline := func() {
		if content.Len() > 0 && lastByte != '\n' {
			content.WriteByte('\n')
			lastByte = '\n'
		}
	}
	write := func(text string) {
		content.WriteString(text)
		if length := len(text); length > 0 {
			lastByte = text[length-1]
		}
	}
	for _, event := range events {
		switch event.Kind {
		case aistudio.EventText:
			write(event.Text)
		case aistudio.EventMedia:
			if event.Media == nil {
				continue
			}
			ensureNewline()
			write(renderMediaMarkdown(*event.Media))
		case aistudio.EventExecutableCode, aistudio.EventCodeExecutionResult:
			rendered := renderCodeExecution(event)
			if rendered == "" {
				continue
			}
			ensureNewline()
			write(rendered)
			content.WriteByte('\n')
			lastByte = '\n'
		}
	}
	return content.String()
}
