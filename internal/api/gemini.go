package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

type geminiRequest struct {
	Contents          []geminiContent        `json:"contents"`
	SystemInstruction *geminiContent         `json:"systemInstruction"`
	GenerationConfig  geminiGenerationConfig `json:"generationConfig"`
	Tools             []geminiToolGroup      `json:"tools"`
	ToolConfig        geminiToolConfig       `json:"toolConfig"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             *string `json:"text"`
	Thought          bool    `json:"thought"`
	ThoughtSignature string  `json:"thoughtSignature"`
	InlineData       *struct {
		MIMEType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData"`
	FileData *struct {
		MIMEType    string `json:"mimeType"`
		FileURI     string `json:"fileUri"`
		DisplayName string `json:"displayName"`
	} `json:"fileData"`
	FunctionCall *struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall"`
	FunctionResponse *struct {
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
	} `json:"functionResponse"`
	ExecutableCode *struct {
		Language string `json:"language"`
		Code     string `json:"code"`
	} `json:"executableCode"`
	CodeExecutionResult *struct {
		Outcome string `json:"outcome"`
		Output  string `json:"output"`
		Error   string `json:"error"`
	} `json:"codeExecutionResult"`
}

type geminiGenerationConfig struct {
	Temperature         *float64                   `json:"temperature"`
	TopP                *float64                   `json:"topP"`
	TopK                *int                       `json:"topK"`
	FrequencyPenalty    *float64                   `json:"frequencyPenalty"`
	PresencePenalty     *float64                   `json:"presencePenalty"`
	CandidateCount      *int64                     `json:"candidateCount"`
	ResponseLogprobs    *bool                      `json:"responseLogprobs"`
	Logprobs            *int64                     `json:"logprobs"`
	MaxOutputTokens     *int64                     `json:"maxOutputTokens"`
	StopSequences       []string                   `json:"stopSequences"`
	ResponseMIMEType    string                     `json:"responseMimeType"`
	ResponseSchema      json.RawMessage            `json:"responseSchema"`
	ResponseJSONSchema  json.RawMessage            `json:"responseJsonSchema"`
	ResponseModalities  []string                   `json:"responseModalities"`
	ImageConfig         *geminiImageConfig         `json:"imageConfig"`
	SpeechConfig        *geminiSpeechConfig        `json:"speechConfig"`
	TranscriptionConfig *geminiTranscriptionConfig `json:"transcriptionConfig"`
	Seed                *int64                     `json:"seed"`
	ThinkingConfig      *struct {
		ThinkingBudget *int64 `json:"thinkingBudget"`
		ThinkingLevel  string `json:"thinkingLevel"`
	} `json:"thinkingConfig"`
}

type geminiTranscriptionConfig struct {
	LanguageCodes      []string `json:"languageCodes"`
	CustomVocabulary   []string `json:"customVocabulary"`
	WordTimestamps     *bool    `json:"wordTimestamps"`
	SpeakerLabels      *bool    `json:"speakerLabels"`
	SmartTranscription bool     `json:"smartTranscription"`
}

type geminiImageConfig struct {
	AspectRatio string `json:"aspectRatio"`
	ImageSize   string `json:"imageSize"`
}

type geminiVoiceConfig struct {
	PrebuiltVoiceConfig *struct {
		VoiceName string `json:"voiceName"`
	} `json:"prebuiltVoiceConfig"`
}

type geminiSpeakerVoiceConfig struct {
	Speaker     string            `json:"speaker"`
	VoiceConfig geminiVoiceConfig `json:"voiceConfig"`
}

type geminiSpeechConfig struct {
	VoiceConfig             *geminiVoiceConfig `json:"voiceConfig"`
	MultiSpeakerVoiceConfig *struct {
		SpeakerVoiceConfigs []geminiSpeakerVoiceConfig `json:"speakerVoiceConfigs"`
	} `json:"multiSpeakerVoiceConfig"`
}

type geminiToolGroup struct {
	FunctionDeclarations []struct {
		Name                 string          `json:"name"`
		Description          string          `json:"description"`
		Parameters           json.RawMessage `json:"parameters"`
		ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
	} `json:"functionDeclarations"`
	GoogleSearch          json.RawMessage `json:"googleSearch"`
	GoogleSearchRetrieval json.RawMessage `json:"googleSearchRetrieval"`
	URLContext            json.RawMessage `json:"urlContext"`
	CodeExecution         json.RawMessage `json:"codeExecution"`
	GoogleMaps            json.RawMessage `json:"googleMaps"`
	ImageSearch           json.RawMessage `json:"imageSearch"`
}

type geminiToolConfig struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames"`
	} `json:"functionCallingConfig"`
}

func (s *server) handleGeminiModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.service.Models(r.Context())
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeGeminiError(w, statusFromError(err), geminiErrorStatus(err), err.Error())
		}
		return
	}
	data := make([]geminiModelOut, 0, len(models))
	for _, model := range models {
		data = append(data, geminiModelObject(model))
	}
	writeJSON(w, http.StatusOK, geminiModelsListOut{Models: data})
}

func (s *server) handleGeminiModel(w http.ResponseWriter, r *http.Request) {
	modelID := strings.TrimPrefix(r.PathValue("model"), "models/")
	models, err := s.service.Models(r.Context())
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeGeminiError(w, statusFromError(err), geminiErrorStatus(err), err.Error())
		}
		return
	}
	for _, model := range models {
		if model.ID == modelID {
			writeJSON(w, http.StatusOK, geminiModelObject(model))
			return
		}
	}
	writeGeminiError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("model %q is unavailable", modelID))
}

func geminiModelObject(model aistudio.Model) geminiModelOut {
	return geminiModelOut{
		Name:                       "models/" + model.ID,
		DisplayName:                model.Name,
		Description:                model.Description,
		SupportedGenerationMethods: model.Methods,
		InputTokenLimit:            model.InputTokenLimit,
		OutputTokenLimit:           model.OutputTokenLimit,
		Capabilities:               model.Capabilities,
		CapabilityOptions:          model.CapabilityOptions,
		AccessModes:                model.AccessModes,
		Paid:                       model.Paid,
	}
}

func (s *server) handleGeminiAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	separator := strings.LastIndex(action, ":")
	if separator < 1 {
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "expected models/{model}:{method}")
		return
	}
	model := strings.TrimPrefix(action[:separator], "models/")
	method := action[separator+1:]
	if method == "predictLongRunning" {
		s.handleGeminiVideoCreate(w, r, model)
		return
	}
	var request geminiRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	if len(request.Contents) == 0 {
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "contents is required")
		return
	}
	generateRequest, err := request.toGenerateRequest(newID("resp"), model)
	if err != nil {
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	switch method {
	case "countTokens":
		s.handleGeminiCountTokens(w, r, generateRequest)
	case "generateContent":
		s.handleGeminiGenerate(w, r, generateRequest, false)
	case "streamGenerateContent":
		s.handleGeminiGenerate(w, r, generateRequest, true)
	default:
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "unknown method: "+method)
	}
}

func (request geminiRequest) toGenerateRequest(id string, model string) (aistudio.GenerateRequest, error) {
	if err := request.GenerationConfig.validate(); err != nil {
		return aistudio.GenerateRequest{}, err
	}
	var system string
	if request.SystemInstruction != nil {
		parts, _, err := mapGeminiParts(request.SystemInstruction.Parts)
		if err != nil {
			return aistudio.GenerateRequest{}, fmt.Errorf("systemInstruction: %w", err)
		}
		var text strings.Builder
		for _, part := range parts {
			if part.Text == "" && (part.InlineData != nil || part.File != nil || part.FunctionCall != nil || part.FunctionResult != nil) {
				return aistudio.GenerateRequest{}, fmt.Errorf("systemInstruction must contain text")
			}
			text.WriteString(part.Text)
		}
		system = text.String()
	}
	contents := make([]aistudio.Content, 0, len(request.Contents))
	for _, content := range request.Contents {
		parts, hasResult, err := mapGeminiParts(content.Parts)
		if err != nil {
			return aistudio.GenerateRequest{}, err
		}
		if len(parts) == 0 {
			continue
		}
		role, err := geminiRole(content.Role)
		if err != nil {
			return aistudio.GenerateRequest{}, err
		}
		if hasResult {
			role = aistudio.RoleTool
		}
		contents = append(contents, aistudio.Content{Role: role, Parts: parts})
	}
	tools, err := mapGeminiTools(request.Tools, request.ToolConfig)
	if err != nil {
		return aistudio.GenerateRequest{}, err
	}
	config := aistudio.GenerationConfig{
		Temperature:      request.GenerationConfig.Temperature,
		TopP:             request.GenerationConfig.TopP,
		TopK:             request.GenerationConfig.TopK,
		MaxOutputTokens:  request.GenerationConfig.MaxOutputTokens,
		StopSequences:    normalizeStopSequences(request.GenerationConfig.StopSequences),
		ResponseMIMEType: request.GenerationConfig.ResponseMIMEType,
		ResponseSchema:   request.GenerationConfig.ResponseSchema,
		Seed:             request.GenerationConfig.Seed,
	}
	config.ResponseModalities, err = mapGeminiResponseModalities(request.GenerationConfig.ResponseModalities)
	if err != nil {
		return aistudio.GenerateRequest{}, err
	}
	if image := request.GenerationConfig.ImageConfig; image != nil {
		config.ImageConfig = &aistudio.ImageConfig{AspectRatio: image.AspectRatio, ImageSize: image.ImageSize}
	}
	config.SpeechConfig, err = mapGeminiSpeechConfig(request.GenerationConfig.SpeechConfig)
	if err != nil {
		return aistudio.GenerateRequest{}, err
	}
	config.TranscriptionConfig, err = mapGeminiTranscriptionConfig(request.GenerationConfig.TranscriptionConfig)
	if err != nil {
		return aistudio.GenerateRequest{}, err
	}
	if len(request.GenerationConfig.ResponseJSONSchema) > 0 {
		config.ResponseSchema = request.GenerationConfig.ResponseJSONSchema
	}
	if request.GenerationConfig.ThinkingConfig != nil {
		config.ThinkingBudget = request.GenerationConfig.ThinkingConfig.ThinkingBudget
		config.ReasoningEffort = request.GenerationConfig.ThinkingConfig.ThinkingLevel
	}
	return aistudio.GenerateRequest{
		ID: id, Model: model, System: system, Contents: contents, Config: config, Tools: tools,
	}, nil
}

func (config geminiGenerationConfig) validate() error {
	if config.FrequencyPenalty != nil && *config.FrequencyPenalty != 0 {
		return fmt.Errorf("generationConfig.frequencyPenalty must be 0")
	}
	if config.PresencePenalty != nil && *config.PresencePenalty != 0 {
		return fmt.Errorf("generationConfig.presencePenalty must be 0")
	}
	if config.CandidateCount != nil && *config.CandidateCount != 1 {
		return fmt.Errorf("generationConfig.candidateCount must be 1")
	}
	if config.ResponseLogprobs != nil && *config.ResponseLogprobs {
		return fmt.Errorf("generationConfig.responseLogprobs must be false")
	}
	if config.Logprobs != nil && *config.Logprobs != 0 {
		return fmt.Errorf("generationConfig.logprobs must be 0")
	}
	return nil
}

func mapGeminiTranscriptionConfig(input *geminiTranscriptionConfig) (*aistudio.TranscriptionConfig, error) {
	if input == nil {
		return nil, nil
	}
	config := &aistudio.TranscriptionConfig{
		SmartTranscription: input.SmartTranscription,
	}
	if input.WordTimestamps != nil {
		config.WordTimestamps = *input.WordTimestamps
	}
	if input.SpeakerLabels != nil {
		config.SpeakerLabels = *input.SpeakerLabels
	}
	for _, vocabulary := range input.CustomVocabulary {
		if vocabulary = strings.TrimSpace(vocabulary); vocabulary != "" {
			config.CustomVocabulary = append(config.CustomVocabulary, vocabulary)
		}
	}
	for _, language := range input.LanguageCodes {
		language = strings.TrimSpace(language)
		if language != "" && !strings.EqualFold(language, "detect") {
			config.LanguageCodes = append(config.LanguageCodes, language)
		}
	}
	if config.SmartTranscription {
		if input.WordTimestamps != nil && *input.WordTimestamps || input.SpeakerLabels != nil && *input.SpeakerLabels {
			return nil, fmt.Errorf("transcriptionConfig.smartTranscription cannot be combined with wordTimestamps or speakerLabels")
		}
		config.WordTimestamps = false
		config.SpeakerLabels = false
	}
	return config, nil
}

func mapGeminiResponseModalities(input []string) ([]aistudio.ResponseModality, error) {
	if input == nil {
		return nil, nil
	}
	modalities := make([]aistudio.ResponseModality, 0, len(input))
	for _, raw := range input {
		modality := aistudio.ResponseModality(strings.ToUpper(strings.TrimSpace(raw)))
		switch modality {
		case aistudio.ResponseModalityText, aistudio.ResponseModalityImage, aistudio.ResponseModalityAudio:
			modalities = append(modalities, modality)
		default:
			return nil, fmt.Errorf("unsupported response modality %q", raw)
		}
	}
	return modalities, nil
}

func mapGeminiSpeechConfig(input *geminiSpeechConfig) (*aistudio.SpeechConfig, error) {
	if input == nil {
		return nil, nil
	}
	if input.VoiceConfig != nil && input.MultiSpeakerVoiceConfig != nil {
		return nil, fmt.Errorf("speechConfig cannot contain both voiceConfig and multiSpeakerVoiceConfig")
	}
	config := &aistudio.SpeechConfig{}
	if input.VoiceConfig != nil {
		if input.VoiceConfig.PrebuiltVoiceConfig == nil || strings.TrimSpace(input.VoiceConfig.PrebuiltVoiceConfig.VoiceName) == "" {
			return nil, fmt.Errorf("speechConfig.voiceConfig requires prebuiltVoiceConfig.voiceName")
		}
		config.VoiceName = input.VoiceConfig.PrebuiltVoiceConfig.VoiceName
	}
	if input.MultiSpeakerVoiceConfig != nil {
		for index, speaker := range input.MultiSpeakerVoiceConfig.SpeakerVoiceConfigs {
			if strings.TrimSpace(speaker.Speaker) == "" || speaker.VoiceConfig.PrebuiltVoiceConfig == nil || strings.TrimSpace(speaker.VoiceConfig.PrebuiltVoiceConfig.VoiceName) == "" {
				return nil, fmt.Errorf("speechConfig.multiSpeakerVoiceConfig.speakerVoiceConfigs[%d] requires speaker and voiceName", index)
			}
			config.Speakers = append(config.Speakers, aistudio.SpeakerVoiceConfig{
				Speaker: speaker.Speaker, VoiceName: speaker.VoiceConfig.PrebuiltVoiceConfig.VoiceName,
			})
		}
	}
	return config, nil
}

func geminiRole(role string) (aistudio.Role, error) {
	switch role {
	case "", "user":
		return aistudio.RoleUser, nil
	case "model", "assistant":
		return aistudio.RoleAssistant, nil
	case "function", "tool":
		return aistudio.RoleTool, nil
	default:
		return "", fmt.Errorf("unsupported content role %q", role)
	}
}

func mapGeminiParts(input []geminiPart) ([]aistudio.Part, bool, error) {
	parts := make([]aistudio.Part, 0, len(input))
	hasResult := false
	for index, part := range input {
		variants := 0
		if part.Text != nil {
			variants++
		}
		if part.InlineData != nil {
			variants++
		}
		if part.FileData != nil {
			variants++
		}
		if part.FunctionCall != nil {
			variants++
		}
		if part.FunctionResponse != nil {
			variants++
		}
		if part.ExecutableCode != nil {
			variants++
		}
		if part.CodeExecutionResult != nil {
			variants++
		}
		if variants == 0 && part.ThoughtSignature != "" {
			parts = append(parts, aistudio.Part{ThoughtSignature: part.ThoughtSignature})
			continue
		}
		if variants != 1 {
			return nil, false, fmt.Errorf("parts[%d] must contain exactly one data field", index)
		}
		switch {
		case part.InlineData != nil:
			if part.InlineData.MIMEType == "" || part.InlineData.Data == "" {
				return nil, false, fmt.Errorf("inlineData requires mimeType and data")
			}
			data, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
			if err != nil {
				return nil, false, fmt.Errorf("inlineData.data: %w", err)
			}
			parts = append(parts, aistudio.Part{
				InlineData:       &aistudio.Blob{MIME: part.InlineData.MIMEType, Data: data},
				ThoughtSignature: part.ThoughtSignature,
			})
		case part.FileData != nil:
			if part.FileData.FileURI == "" || part.FileData.MIMEType == "" {
				return nil, false, fmt.Errorf("fileData requires fileUri and mimeType")
			}
			if media, ok := aistudio.ExternalMediaForURL(part.FileData.FileURI); ok {
				parts = append(parts, aistudio.Part{ExternalMedia: media, ThoughtSignature: part.ThoughtSignature})
			} else {
				parts = append(parts, aistudio.Part{
					File: &aistudio.FileRef{
						ID: part.FileData.FileURI, Name: part.FileData.DisplayName, MIME: part.FileData.MIMEType,
					},
					ThoughtSignature: part.ThoughtSignature,
				})
			}
		case part.FunctionCall != nil:
			if part.FunctionCall.Name == "" {
				return nil, false, fmt.Errorf("functionCall requires name")
			}
			arguments, err := geminiJSONObject(part.FunctionCall.Args, "functionCall.args")
			if err != nil {
				return nil, false, err
			}
			parts = append(parts, aistudio.Part{
				FunctionCall: &aistudio.FunctionCall{
					ID: part.FunctionCall.ID, Name: part.FunctionCall.Name, Arguments: arguments, ThoughtSignature: part.ThoughtSignature,
				},
				ThoughtSignature: part.ThoughtSignature,
			})
		case part.FunctionResponse != nil:
			if part.FunctionResponse.Name == "" {
				return nil, false, fmt.Errorf("functionResponse requires name")
			}
			response, err := geminiJSONObject(part.FunctionResponse.Response, "functionResponse.response")
			if err != nil {
				return nil, false, err
			}
			parts = append(parts, aistudio.Part{
				FunctionResult: &aistudio.FunctionResult{
					ID: part.FunctionResponse.ID, Name: part.FunctionResponse.Name, Content: response,
				},
				ThoughtSignature: part.ThoughtSignature,
			})
			hasResult = true
		case part.ExecutableCode != nil:
			parts = append(parts, aistudio.Part{
				ExecutableCode: &aistudio.ExecutableCode{
					Language: part.ExecutableCode.Language, Code: part.ExecutableCode.Code,
				},
				ThoughtSignature: part.ThoughtSignature,
			})
		case part.CodeExecutionResult != nil:
			parts = append(parts, aistudio.Part{
				CodeExecutionResult: &aistudio.CodeExecutionResult{
					Outcome: part.CodeExecutionResult.Outcome,
					Output:  part.CodeExecutionResult.Output,
					Error:   part.CodeExecutionResult.Error,
				},
				ThoughtSignature: part.ThoughtSignature,
			})
		default:
			if part.Thought {
				if part.ThoughtSignature != "" {
					parts = append(parts, aistudio.Part{ThoughtSignature: part.ThoughtSignature})
				}
				continue
			}
			parts = append(parts, aistudio.Part{Text: *part.Text, ThoughtSignature: part.ThoughtSignature})
		}
	}
	return parts, hasResult, nil
}

func geminiJSONObject(raw json.RawMessage, field string) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%s must be an object", field)
	}
	return raw, nil
}

func mapGeminiGoogleSearch(raw json.RawMessage) (*aistudio.GoogleSearchOptions, error) {
	if !geminiRawObjectPresent(raw) {
		return nil, nil
	}
	var config struct {
		SearchTypes *struct {
			WebSearch   json.RawMessage `json:"webSearch"`
			ImageSearch json.RawMessage `json:"imageSearch"`
		} `json:"searchTypes"`
		TimeRangeFilter *struct {
			StartTime string `json:"startTime"`
			EndTime   string `json:"endTime"`
		} `json:"timeRangeFilter"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("googleSearch must be an object")
	}
	options := &aistudio.GoogleSearchOptions{}
	if config.SearchTypes == nil {
		options.WebSearch = true
	} else {
		options.WebSearch = geminiRawObjectPresent(config.SearchTypes.WebSearch)
		options.ImageSearch = geminiRawObjectPresent(config.SearchTypes.ImageSearch)
		if !options.WebSearch && !options.ImageSearch {
			options.WebSearch = true
		}
	}
	if config.TimeRangeFilter != nil {
		timeRange := &aistudio.GoogleSearchTimeRange{}
		if config.TimeRangeFilter.StartTime != "" {
			value, err := time.Parse(time.RFC3339Nano, config.TimeRangeFilter.StartTime)
			if err != nil {
				return nil, fmt.Errorf("googleSearch.timeRangeFilter.startTime: %w", err)
			}
			timeRange.StartTime = value
		}
		if config.TimeRangeFilter.EndTime != "" {
			value, err := time.Parse(time.RFC3339Nano, config.TimeRangeFilter.EndTime)
			if err != nil {
				return nil, fmt.Errorf("googleSearch.timeRangeFilter.endTime: %w", err)
			}
			timeRange.EndTime = value
		}
		options.TimeRange = timeRange
	}
	return options, nil
}

func geminiRawObjectPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && strings.TrimSpace(string(raw)) != "null"
}

func mapGeminiTools(groups []geminiToolGroup, config geminiToolConfig) (aistudio.Tools, error) {
	var mapped aistudio.Tools
	for _, group := range groups {
		for _, declaration := range group.FunctionDeclarations {
			if declaration.Name == "" {
				return aistudio.Tools{}, fmt.Errorf("function declaration name is required")
			}
			parameters := declaration.Parameters
			if len(declaration.ParametersJSONSchema) > 0 {
				parameters = declaration.ParametersJSONSchema
			}
			if len(parameters) == 0 {
				parameters = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			mapped.Functions = append(mapped.Functions, aistudio.FunctionDeclaration{
				Name: declaration.Name, Description: declaration.Description, Parameters: parameters,
			})
		}
		search, err := mapGeminiGoogleSearch(group.GoogleSearch)
		if err != nil {
			return aistudio.Tools{}, err
		}
		if search != nil {
			if mapped.GoogleSearch == nil {
				mapped.GoogleSearch = search
			} else {
				mapped.GoogleSearch.WebSearch = mapped.GoogleSearch.WebSearch || search.WebSearch
				mapped.GoogleSearch.ImageSearch = mapped.GoogleSearch.ImageSearch || search.ImageSearch
				if search.TimeRange != nil {
					mapped.GoogleSearch.TimeRange = search.TimeRange
				}
			}
		}
		retrieval, err := geminiEmptyObjectPresent(group.GoogleSearchRetrieval, "googleSearchRetrieval")
		if err != nil {
			return aistudio.Tools{}, err
		}
		if retrieval {
			mapped.Google = appendUnique(mapped.Google, "google_search")
		}
		if geminiRawObjectPresent(group.URLContext) {
			mapped.Google = appendUnique(mapped.Google, "url_context")
		}
		if geminiRawObjectPresent(group.CodeExecution) {
			mapped.Google = appendUnique(mapped.Google, "code_execution")
		}
		if geminiRawObjectPresent(group.GoogleMaps) {
			mapped.Google = appendUnique(mapped.Google, "google_maps")
		}
		if geminiRawObjectPresent(group.ImageSearch) {
			mapped.Google = appendUnique(mapped.Google, "image_search")
		}
	}
	var toolConfig aistudio.ToolConfig
	if len(config.FunctionCallingConfig.AllowedFunctionNames) > 0 {
		return aistudio.Tools{}, fmt.Errorf("allowedFunctionNames is not supported by AI Studio Web")
	}
	switch strings.ToUpper(config.FunctionCallingConfig.Mode) {
	case "", "AUTO":
		toolConfig.Mode = "auto"
	case "ANY":
		return aistudio.Tools{}, fmt.Errorf("functionCallingConfig mode ANY is not supported by AI Studio Web")
	case "NONE":
		toolConfig.Mode = "none"
	default:
		return aistudio.Tools{}, fmt.Errorf("unsupported functionCallingConfig mode %q", config.FunctionCallingConfig.Mode)
	}
	if len(mapped.Functions) == 0 && len(mapped.Google) == 0 && mapped.GoogleSearch == nil {
		return mapped, nil
	}
	mapped.ToolConfig = toolConfig
	return mapped, nil
}

func geminiEmptyObjectPresent(raw json.RawMessage, field string) (bool, error) {
	if !geminiRawObjectPresent(raw) {
		return false, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return false, fmt.Errorf("%s must be an object", field)
	}
	if len(object) != 0 {
		return false, fmt.Errorf("%s only accepts an empty object", field)
	}
	return true, nil
}

func (s *server) handleGeminiCountTokens(w http.ResponseWriter, r *http.Request, request aistudio.GenerateRequest) {
	count, err := s.service.CountTokens(r.Context(), aistudio.TokenCountRequest{
		Model: request.Model, System: request.System, Contents: request.Contents, Tools: request.Tools,
	})
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeGeminiError(w, statusFromError(err), geminiErrorStatus(err), err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, geminiCountTokensOut{TotalTokens: count.InputTokens})
}

func (s *server) handleGeminiGenerate(w http.ResponseWriter, r *http.Request, request aistudio.GenerateRequest, stream bool) {
	events, err := s.service.Generate(r.Context(), request)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeGeminiError(w, statusFromError(err), geminiErrorStatus(err), err.Error())
		}
		return
	}
	if stream {
		s.streamGemini(w, r, request, events)
		return
	}
	result, err := consumeEvents(r.Context(), events, nil)
	if err != nil {
		if shouldWriteRequestError(r, err) {
			writeGeminiError(w, statusFromError(err), geminiErrorStatus(err), err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, buildGeminiResponse(request, result))
}

func buildGeminiResponse(request aistudio.GenerateRequest, result generationResult) geminiGenerateResponseOut {
	candidate := geminiCandidateOut{
		Content: &geminiContentOut{Role: "model", Parts: geminiOutputParts(result)},
		Index:   0,
	}
	setGeminiFinish(&candidate, result.finishReason)
	if result.grounding != nil {
		candidate.GroundingMetadata = geminiGroundingMetadata(*result.grounding)
	} else if len(result.citations) > 0 {
		candidate.CitationMetadata = geminiCitationMetadata(result.citations)
	}
	model := request.Model
	if result.providerModel != "" {
		model = result.providerModel
	}
	response := geminiGenerateResponseOut{
		Candidates:   []geminiCandidateOut{candidate},
		ModelVersion: model,
		ResponseID:   request.ID,
	}
	if result.usage != nil {
		response.UsageMetadata = geminiUsage(result.usage)
	}
	return response
}

func geminiOutputParts(result generationResult) []geminiPartOut {
	parts := make([]geminiPartOut, 0)
	for _, event := range result.events {
		switch event.Kind {
		case aistudio.EventText:
			parts = append(parts, geminiSignedPart(geminiTextPart(event), event.ThoughtSignature))
		case aistudio.EventReasoning:
			text := event.Text
			parts = append(parts, geminiSignedPart(geminiPartOut{Text: &text, Thought: true}, event.ThoughtSignature))
		case aistudio.EventToolCall:
			if event.ToolCall != nil {
				parts = append(parts, geminiSignedPart(geminiFunctionCallPart(*event.ToolCall), event.ThoughtSignature))
			}
		case aistudio.EventExecutableCode:
			if event.ExecutableCode != nil {
				parts = append(parts, geminiSignedPart(geminiPartOut{ExecutableCode: &geminiExecutableCodeOut{
					Language: event.ExecutableCode.Language, Code: event.ExecutableCode.Code,
				}}, event.ThoughtSignature))
			}
		case aistudio.EventCodeExecutionResult:
			if event.CodeExecutionResult != nil {
				parts = append(parts, geminiSignedPart(geminiPartOut{
					CodeExecutionResult: geminiCodeExecutionResult(*event.CodeExecutionResult),
				}, event.ThoughtSignature))
			}
		case aistudio.EventMedia:
			if event.Media != nil {
				if len(event.Media.Data) > 0 {
					parts = append(parts, geminiSignedPart(geminiPartOut{InlineData: &geminiInlineDataOut{
						MIMEType: event.Media.MIME, Data: base64.StdEncoding.EncodeToString(event.Media.Data),
					}}, event.ThoughtSignature))
				} else if event.Media.URL != "" {
					parts = append(parts, geminiSignedPart(geminiPartOut{FileData: &geminiFileDataOut{
						MIMEType: event.Media.MIME, FileURI: event.Media.URL, DisplayName: event.Media.Name,
					}}, event.ThoughtSignature))
				}
			}
		case aistudio.EventThoughtSignature:
			if event.ThoughtSignature != "" {
				parts = append(parts, geminiPartOut{ThoughtSignature: event.ThoughtSignature})
			}
		}
	}
	return parts
}

func geminiTextPart(event aistudio.Event) geminiPartOut {
	text := event.Text
	part := geminiPartOut{Text: &text}
	if event.Transcript == nil {
		return part
	}
	metadata := &geminiTranscriptionMetaOut{Speaker: event.Transcript.Speaker}
	if len(event.Transcript.Timestamps) > 0 {
		timestamps := make([]geminiTranscriptRangeOut, 0, len(event.Transcript.Timestamps))
		for _, timestamp := range event.Transcript.Timestamps {
			timestamps = append(timestamps, geminiTranscriptRangeOut{
				Start: geminiTranscriptDuration(timestamp.Start),
				End:   geminiTranscriptDuration(timestamp.End),
			})
		}
		metadata.Timestamps = timestamps
	}
	part.TranscriptionMetadata = metadata
	return part
}
func geminiTranscriptDuration(duration aistudio.TranscriptDuration) geminiTranscriptDurationOut {
	return geminiTranscriptDurationOut{Seconds: duration.Seconds, Nanos: duration.Nanos}
}
func geminiFunctionCallPart(call aistudio.FunctionCall) geminiPartOut {
	part := geminiPartOut{FunctionCall: &geminiFunctionCallOut{
		ID: call.ID, Name: call.Name, Args: call.Arguments,
	}}
	if call.ThoughtSignature != "" {
		part.ThoughtSignature = call.ThoughtSignature
	}
	return part
}
func geminiSignedPart(part geminiPartOut, signature string) geminiPartOut {
	if signature != "" {
		part.ThoughtSignature = signature
	}
	return part
}
func geminiCodeExecutionResult(result aistudio.CodeExecutionResult) *geminiCodeExecutionResultOut {
	output := &geminiCodeExecutionResultOut{Outcome: result.Outcome}
	if result.Outcome == "OUTCOME_OK" {
		output.Output = &result.Output
	} else {
		output.Error = &result.Error
	}
	return output
}
func geminiCitationMetadata(citations []aistudio.Citation) *geminiCitationMetaOut {
	sources := make([]geminiCitationSourceOut, 0, len(citations))
	for _, citation := range citations {
		sources = append(sources, geminiCitationSourceOut{
			URI: citation.URL, Title: citation.Title, StartIndex: citation.Start, EndIndex: citation.End,
		})
	}
	return &geminiCitationMetaOut{CitationSources: sources}
}
func geminiGroundingMetadata(metadata aistudio.GroundingMetadata) *geminiGroundingMetaOut {
	output := &geminiGroundingMetaOut{}
	if metadata.SearchEntryPoint != nil {
		output.SearchEntryPoint = &geminiSearchEntryOut{
			RenderedContent: metadata.SearchEntryPoint.RenderedContent,
			SDKBlob:         metadata.SearchEntryPoint.SDKBlob,
		}
	}
	if len(metadata.Chunks) > 0 {
		chunks := make([]geminiGroundingChunkOut, 0, len(metadata.Chunks))
		for _, chunk := range metadata.Chunks {
			switch chunk.Source {
			case "web":
				chunks = append(chunks, geminiGroundingChunkOut{Web: &geminiChunkWebOut{URI: chunk.URI, Title: chunk.Title}})
			case "retrieved_context":
				chunks = append(chunks, geminiGroundingChunkOut{RetrievedContext: &geminiChunkRetrievedOut{
					URI: chunk.URI, Title: chunk.Title, Text: chunk.Text,
				}})
			case "maps":
				chunks = append(chunks, geminiGroundingChunkOut{Maps: &geminiChunkMapsOut{
					URI: chunk.URI, Title: chunk.Title, Text: chunk.Text, PlaceID: chunk.PlaceID,
				}})
			}
		}
		output.GroundingChunks = chunks
	}
	if len(metadata.Supports) > 0 {
		supports := make([]geminiGroundingSupportOut, 0, len(metadata.Supports))
		for _, support := range metadata.Supports {
			item := geminiGroundingSupportOut{
				Segment: geminiSegmentOut{
					PartIndex: support.Segment.PartIndex, StartIndex: support.Segment.StartIndex,
					EndIndex: support.Segment.EndIndex, Text: support.Segment.Text,
				},
				GroundingChunkIndices: support.ChunkIndices,
			}
			if len(support.ConfidenceScores) > 0 {
				item.ConfidenceScores = support.ConfidenceScores
			}
			supports = append(supports, item)
		}
		output.GroundingSupports = supports
	}
	if metadata.DynamicRetrievalScore != nil {
		output.RetrievalMetadata = &geminiRetrievalMetaOut{
			GoogleSearchDynamicRetrievalScore: *metadata.DynamicRetrievalScore,
		}
	}
	output.WebSearchQueries = metadata.WebSearchQueries
	if metadata.MapsWidgetContextToken != "" {
		output.GoogleMapsWidgetContextToken = metadata.MapsWidgetContextToken
	}
	return output
}
func geminiFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "", "stop", "stop_sequence":
		return "STOP"
	case "unspecified":
		return "FINISH_REASON_UNSPECIFIED"
	case "max_tokens", "max_output_tokens", "length":
		return "MAX_TOKENS"
	case "safety", "content_filter", "blocked":
		return "SAFETY"
	case "recitation":
		return "RECITATION"
	case "language":
		return "LANGUAGE"
	case "other":
		return "OTHER"
	case "blocklist":
		return "BLOCKLIST"
	case "prohibited_content":
		return "PROHIBITED_CONTENT"
	case "spii":
		return "SPII"
	case "malformed_function_call":
		return "MALFORMED_FUNCTION_CALL"
	case "image_safety":
		return "IMAGE_SAFETY"
	case "unexpected_tool_call":
		return "UNEXPECTED_TOOL_CALL"
	case "too_many_tool_calls":
		return "TOO_MANY_TOOL_CALLS"
	case "image_prohibited_content":
		return "IMAGE_PROHIBITED_CONTENT"
	case "image_other":
		return "IMAGE_OTHER"
	case "no_image":
		return "NO_IMAGE"
	case "image_recitation":
		return "IMAGE_RECITATION"
	case "missing_thought_signature":
		return "OTHER"
	default:
		return "OTHER"
	}
}

func setGeminiFinish(candidate *geminiCandidateOut, reason string) {
	candidate.FinishReason = geminiFinishReason(reason)
	normalized := strings.ToLower(strings.TrimSpace(reason))
	if normalized == "missing_thought_signature" {
		candidate.FinishMessage = "Missing thought signature"
	} else if strings.HasPrefix(normalized, "provider_") {
		candidate.FinishMessage = "AI Studio finish reason " + strings.TrimPrefix(normalized, "provider_")
	}
}
func geminiUsage(usage *aistudio.Usage) *geminiUsageMetadataOut {
	return &geminiUsageMetadataOut{
		PromptTokenCount:        usage.InputTokens,
		CandidatesTokenCount:    usage.OutputTokens,
		ThoughtsTokenCount:      usage.ReasoningTokens,
		ToolUsePromptTokenCount: usage.ToolTokens,
		TotalTokenCount:         usage.TotalTokens,
	}
}
func (s *server) streamGemini(w http.ResponseWriter, r *http.Request, request aistudio.GenerateRequest, events <-chan aistudio.Event) {
	streamHeaders(w)
	result, err := consumeStreamEvents(r.Context(), events, func(event aistudio.Event) error {
		response := geminiStreamChunkOut{ResponseID: request.ID, ModelVersion: request.Model}
		switch event.Kind {
		case aistudio.EventText:
			response.Candidates = []geminiCandidateOut{geminiStreamCandidate(geminiSignedPart(geminiTextPart(event), event.ThoughtSignature))}
		case aistudio.EventReasoning:
			text := event.Text
			response.Candidates = []geminiCandidateOut{geminiStreamCandidate(geminiSignedPart(geminiPartOut{Text: &text, Thought: true}, event.ThoughtSignature))}
		case aistudio.EventToolCall:
			if event.ToolCall == nil {
				return nil
			}
			response.Candidates = []geminiCandidateOut{geminiStreamCandidate(geminiSignedPart(geminiFunctionCallPart(*event.ToolCall), event.ThoughtSignature))}
		case aistudio.EventExecutableCode:
			if event.ExecutableCode == nil {
				return nil
			}
			part := geminiPartOut{ExecutableCode: &geminiExecutableCodeOut{
				Language: event.ExecutableCode.Language, Code: event.ExecutableCode.Code,
			}}
			response.Candidates = []geminiCandidateOut{geminiStreamCandidate(geminiSignedPart(part, event.ThoughtSignature))}
		case aistudio.EventCodeExecutionResult:
			if event.CodeExecutionResult == nil {
				return nil
			}
			part := geminiPartOut{
				CodeExecutionResult: geminiCodeExecutionResult(*event.CodeExecutionResult),
			}
			response.Candidates = []geminiCandidateOut{geminiStreamCandidate(geminiSignedPart(part, event.ThoughtSignature))}
		case aistudio.EventGrounding:
			if event.Grounding == nil {
				return nil
			}
			response.Candidates = []geminiCandidateOut{{
				Index: 0, GroundingMetadata: geminiGroundingMetadata(*event.Grounding),
			}}
		case aistudio.EventCitation:
			if event.Citation == nil {
				return nil
			}
			response.Candidates = []geminiCandidateOut{{
				Index: 0, CitationMetadata: geminiCitationMetadata([]aistudio.Citation{*event.Citation}),
			}}
		case aistudio.EventMedia:
			if event.Media == nil {
				return nil
			}
			var part geminiPartOut
			if len(event.Media.Data) > 0 {
				part = geminiPartOut{InlineData: &geminiInlineDataOut{
					MIMEType: event.Media.MIME, Data: base64.StdEncoding.EncodeToString(event.Media.Data),
				}}
			} else if event.Media.URL != "" {
				part = geminiPartOut{FileData: &geminiFileDataOut{
					MIMEType: event.Media.MIME, FileURI: event.Media.URL, DisplayName: event.Media.Name,
				}}
			} else {
				return nil
			}
			response.Candidates = []geminiCandidateOut{geminiStreamCandidate(geminiSignedPart(part, event.ThoughtSignature))}
		case aistudio.EventThoughtSignature:
			if event.ThoughtSignature == "" {
				return nil
			}
			response.Candidates = []geminiCandidateOut{geminiStreamCandidate(geminiPartOut{ThoughtSignature: event.ThoughtSignature})}
		default:
			return nil
		}
		return writeSSE(w, "", response)
	}, func() error { return writeSSEHeartbeat(w) })
	if err != nil {
		if shouldWriteRequestError(r, err) {
			_ = writeSSE(w, "", geminiStreamErrorOut{Error: geminiErrorInfoOut{
				Code: statusFromError(err), Message: err.Error(), Status: geminiErrorStatus(err),
			}})
		}
		return
	}
	model := request.Model
	if result.providerModel != "" {
		model = result.providerModel
	}
	final := geminiStreamChunkOut{
		ResponseID: request.ID, ModelVersion: model,
		Candidates: []geminiCandidateOut{{Index: 0}},
	}
	setGeminiFinish(&final.Candidates[0], result.finishReason)
	if result.usage != nil {
		final.UsageMetadata = geminiUsage(result.usage)
	}
	_ = writeSSE(w, "", final)
}
func geminiStreamCandidate(part geminiPartOut) geminiCandidateOut {
	return geminiCandidateOut{Index: 0, Content: &geminiContentOut{Role: "model", Parts: []geminiPartOut{part}}}
}
