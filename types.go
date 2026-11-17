package main

import "encoding/json"

// ---- OpenAI Chat Completions ----

type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type FunctionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type Tool struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ResponseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
	} `json:"json_schema,omitempty"`
}

type ChatCompletionRequest struct {
	Model             string          `json:"model"`
	Messages          []ChatMessage   `json:"messages"`
	Stream            bool            `json:"stream,omitempty"`
	StreamOptions     *StreamOptions  `json:"stream_options,omitempty"`
	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   string          `json:"reasoning_effort,omitempty"`
	WebSearchOptions  json.RawMessage `json:"web_search_options,omitempty"`
	ResponseFormat    *ResponseFormat `json:"response_format,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type URLCitation struct {
	Type       string `json:"type"`
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
	Title      string `json:"title"`
	URL        string `json:"url"`
}

type ChatAnnotation struct {
	Type        string `json:"type"`
	URLCitation struct {
		StartIndex int    `json:"start_index"`
		EndIndex   int    `json:"end_index"`
		Title      string `json:"title"`
		URL        string `json:"url"`
	} `json:"url_citation"`
}

type ResponseMessage struct {
	Role        string           `json:"role"`
	Content     *string          `json:"content"`
	ToolCalls   []ToolCall       `json:"tool_calls,omitempty"`
	Annotations []ChatAnnotation `json:"annotations,omitempty"`
}

type Choice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

type Delta struct {
	Role        string           `json:"role,omitempty"`
	Content     string           `json:"content,omitempty"`
	ToolCalls   []ToolCall       `json:"tool_calls,omitempty"`
	Annotations []ChatAnnotation `json:"annotations,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

type APIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

type APIError struct {
	Error APIErrorBody `json:"error"`
}

// ---- OpenAI Responses ----

type InputItem struct {
	Type      string          `json:"type,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
}

type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ResponsesRequest struct {
	Model              string          `json:"model"`
	Instructions       string          `json:"instructions,omitempty"`
	Input              json.RawMessage `json:"input"`
	Stream             bool            `json:"stream,omitempty"`
	Tools              []ResponsesTool `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	Reasoning          *struct {
		Effort string `json:"effort"`
	} `json:"reasoning,omitempty"`
	Text *struct {
		Format *struct {
			Type   string          `json:"type"`
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	} `json:"text,omitempty"`
	Include []string `json:"include,omitempty"`
}

type WebSearchSource struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type WebSearchAction struct {
	Type    string            `json:"type"`
	Query   string            `json:"query,omitempty"`
	URL     string            `json:"url,omitempty"`
	Sources []WebSearchSource `json:"sources,omitempty"`
}

type OutputText struct {
	Type        string        `json:"type"`
	Text        string        `json:"text"`
	Annotations []URLCitation `json:"annotations"`
}

type OutputItem struct {
	Type      string           `json:"type"`
	ID        string           `json:"id"`
	Status    string           `json:"status,omitempty"`
	Role      string           `json:"role,omitempty"`
	Content   *[]OutputText    `json:"content,omitempty"`
	CallID    string           `json:"call_id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Arguments *string          `json:"arguments,omitempty"`
	Action    *WebSearchAction `json:"action,omitempty"`
}

type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type ResponseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Response struct {
	ID                 string          `json:"id"`
	Object             string          `json:"object"`
	CreatedAt          int64           `json:"created_at"`
	Status             string          `json:"status"`
	Model              string          `json:"model"`
	Output             []OutputItem    `json:"output"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Usage              *ResponsesUsage `json:"usage,omitempty"`
	Error              *ResponseError  `json:"error"`
}
