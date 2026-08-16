package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAICompat speaks the OpenAI chat-completions wire format. Every backend
// we target — Cerebras, Groq, OpenRouter, Gemini's compat endpoint, GitHub
// Models, llama-server, Ollama — exposes it, so one client covers the whole
// fleet. Per-provider quirks are capability flags, not separate code paths.
type OpenAICompat struct {
	name    string
	baseURL string
	apiKey  string
	headers map[string]string
	client  *http.Client

	requiresKey bool
	// streamUsage: send stream_options.include_usage. Most providers support
	// it; Gemini's compat shim historically does not, and a rejected request
	// is worse than an estimated token count.
	streamUsage bool
	// jsonSchema: supports response_format.type == "json_schema".
	jsonSchema bool
	// maxTokensField: "max_tokens" for everything free today;
	// "max_completion_tokens" for newer OpenAI-proper endpoints.
	maxTokensField string
}

type OpenAIOpts struct {
	Name           string
	BaseURL        string
	APIKey         string
	Headers        map[string]string
	Timeout        time.Duration
	RequiresKey    bool
	StreamUsage    bool
	JSONSchema     bool
	MaxTokensField string
}

func NewOpenAICompat(o OpenAIOpts) *OpenAICompat {
	if o.Timeout <= 0 {
		o.Timeout = 180 * time.Second
	}
	if o.MaxTokensField == "" {
		o.MaxTokensField = "max_tokens"
	}
	return &OpenAICompat{
		name:           o.Name,
		baseURL:        strings.TrimRight(o.BaseURL, "/"),
		apiKey:         o.APIKey,
		headers:        o.Headers,
		requiresKey:    o.RequiresKey,
		streamUsage:    o.StreamUsage,
		jsonSchema:     o.JSONSchema,
		maxTokensField: o.MaxTokensField,
		client: &http.Client{
			Timeout: o.Timeout,
			Transport: &http.Transport{
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
				// Local llama-server can sit silent for a long time during
				// prefill on CPU; don't let the transport give up early.
				ResponseHeaderTimeout: o.Timeout,
			},
		},
	}
}

func (c *OpenAICompat) Name() string { return c.name }

func (c *OpenAICompat) Configured() bool {
	if c.baseURL == "" {
		return false
	}
	return !c.requiresKey || c.apiKey != ""
}

// buildBody assembles the request as a map so the max-tokens field name can
// vary per provider without duplicating a struct.
func (c *OpenAICompat) buildBody(model string, req Request, stream bool) map[string]any {
	body := map[string]any{
		"model":    model,
		"messages": normalizeMessages(req.Messages),
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if req.TopP > 0 {
		body["top_p"] = req.TopP
	}
	if req.MaxTokens > 0 {
		body[c.maxTokensField] = req.MaxTokens
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
		if req.ToolChoice != "" {
			body["tool_choice"] = req.ToolChoice
		}
	}
	if req.JSONSchema != nil && c.jsonSchema {
		name := req.SchemaName
		if name == "" {
			name = "response"
		}
		body["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   name,
				"strict": true,
				"schema": req.JSONSchema,
			},
		}
	}
	if stream {
		body["stream"] = true
		if c.streamUsage {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	return body
}

// normalizeMessages guarantees a non-nil content field. Several backends
// reject `"content": null` on assistant tool-call turns even though OpenAI
// itself emits exactly that.
func normalizeMessages(in []Message) []Message {
	out := make([]Message, len(in))
	copy(out, in)
	return out
}

func (c *OpenAICompat) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	httpReq.Header.Set("Accept", "application/json")
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}
	return httpReq, nil
}

func (c *OpenAICompat) wrapErr(model string, resp *http.Response, body string, transport error) *Error {
	if transport != nil {
		return &Error{Kind: classifyTransport(transport), Provider: c.name, Model: model, Err: transport}
	}
	retryAfter := parseRetryAfter(resp.Header)
	return &Error{
		Kind:       classifyHTTP(resp.StatusCode, body, retryAfter),
		Provider:   c.name,
		Model:      model,
		Status:     resp.StatusCode,
		Body:       body,
		RetryAfter: retryAfter,
	}
}

// ---------- non-streaming ----------

type wireError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

type wireChoice struct {
	Index   int `json:"index"`
	Message struct {
		Role             string      `json:"role"`
		Content          flexContent `json:"content"`
		ReasoningContent string      `json:"reasoning_content"`
		Reasoning        string      `json:"reasoning"`
		ToolCalls        []ToolCall  `json:"tool_calls"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

type wireResp struct {
	Model   string       `json:"model"`
	Choices []wireChoice `json:"choices"`
	Usage   *Usage       `json:"usage"`
	Error   *wireError   `json:"error"`
}

func (c *OpenAICompat) Chat(ctx context.Context, model string, req Request) (*Response, error) {
	start := time.Now()
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", c.buildBody(model, req, false))
	if err != nil {
		return nil, &Error{Kind: ErrBadRequest, Provider: c.name, Model: model, Err: err}
	}
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, c.wrapErr(model, nil, "", err)
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 400 {
		return nil, c.wrapErr(model, resp, string(raw), nil)
	}
	if readErr != nil {
		return nil, c.wrapErr(model, nil, "", readErr)
	}

	var wr wireResp
	if err := json.Unmarshal(raw, &wr); err != nil {
		return nil, &Error{Kind: ErrUnknown, Provider: c.name, Model: model, Body: truncate(string(raw), 400), Err: err}
	}
	if wr.Error != nil && wr.Error.Message != "" {
		// Some gateways return HTTP 200 with an error object inside.
		return nil, &Error{Kind: classifyHTTP(200, wr.Error.Message, 0), Provider: c.name, Model: model, Body: wr.Error.Message}
	}
	if len(wr.Choices) == 0 {
		return nil, &Error{Kind: ErrUnknown, Provider: c.name, Model: model, Body: "no choices returned"}
	}

	ch := wr.Choices[0]
	// Same treatment as the streaming path: a model that inlines its
	// scratchpad must not have it mistaken for the answer.
	body, thought := SplitThinking(string(ch.Message.Content))
	out := &Response{
		Content:      body,
		Reasoning:    firstNonEmpty(ch.Message.ReasoningContent, ch.Message.Reasoning, thought),
		ToolCalls:    ch.Message.ToolCalls,
		FinishReason: ch.FinishReason,
		Model:        firstNonEmpty(wr.Model, model),
		Provider:     c.name,
		Latency:      time.Since(start),
	}
	if wr.Usage != nil && wr.Usage.TotalTokens > 0 {
		out.Usage = *wr.Usage
	} else {
		out.Usage = EstimateUsage(req, out.Content+out.Reasoning)
	}
	// A non-streaming call has no first-token signal. Leaving TTFT at zero
	// (rather than copying Latency) keeps the throughput helpers honest:
	// they report "no measurement" instead of a fabricated prefill rate.
	return out, nil
}

// ---------- streaming ----------

type wireStreamChoice struct {
	Index int `json:"index"`
	Delta struct {
		Role             string      `json:"role"`
		Content          flexContent `json:"content"`
		ReasoningContent string      `json:"reasoning_content"`
		Reasoning        string      `json:"reasoning"`
		ToolCalls        []struct {
			Index    *int   `json:"index"`
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
			ExtraContent json.RawMessage `json:"extra_content"`
		} `json:"tool_calls"`
	} `json:"delta"`
	FinishReason string `json:"finish_reason"`
}

type wireStreamResp struct {
	Model   string             `json:"model"`
	Choices []wireStreamChoice `json:"choices"`
	Usage   *Usage             `json:"usage"`
	Error   *wireError         `json:"error"`
}

func (c *OpenAICompat) Stream(ctx context.Context, model string, req Request, onChunk func(Chunk) error) (*Response, error) {
	start := time.Now()
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", c.buildBody(model, req, true))
	if err != nil {
		return nil, &Error{Kind: ErrBadRequest, Provider: c.name, Model: model, Err: err}
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, c.wrapErr(model, nil, "", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, c.wrapErr(model, resp, string(raw), nil)
	}

	var (
		content   strings.Builder
		reasoning strings.Builder
		toolFrags = map[int]*ToolCall{}
		usage     *Usage
		finish    string
		gotModel  string
		ttft      time.Duration
		think     thinkSplitter
	)

	// bufio.Reader rather than Scanner: SSE payloads carrying a whole file
	// edit routinely exceed Scanner's default 64 KB token cap.
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, err := readSSELine(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			// A truncated stream after real output is still partial output;
			// return what we have plus the error so the router can decide.
			return nil, c.wrapErr(model, nil, "", err)
		}
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}

		var sr wireStreamResp
		if err := json.Unmarshal([]byte(data), &sr); err != nil {
			continue // tolerate keep-alive or non-conforming frames
		}
		if sr.Error != nil && sr.Error.Message != "" {
			return nil, &Error{Kind: classifyHTTP(200, sr.Error.Message, 0), Provider: c.name, Model: model, Body: sr.Error.Message}
		}
		if sr.Model != "" {
			gotModel = sr.Model
		}
		if sr.Usage != nil && sr.Usage.TotalTokens > 0 {
			u := *sr.Usage
			usage = &u
		}
		if len(sr.Choices) == 0 {
			continue
		}
		ch := sr.Choices[0]
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}

		if txt := string(ch.Delta.Content); txt != "" {
			if ttft == 0 {
				ttft = time.Since(start)
			}
			// Route inline <think> spans to the reasoning channel so they
			// never reach the caller as content.
			body, thought := think.feed(txt)
			if thought != "" {
				reasoning.WriteString(thought)
				if onChunk != nil {
					if err := onChunk(Chunk{Reasoning: thought}); err != nil {
						return nil, err
					}
				}
			}
			if body != "" {
				content.WriteString(body)
				if onChunk != nil {
					if err := onChunk(Chunk{Content: body}); err != nil {
						return nil, err
					}
				}
			}
		}
		if rt := firstNonEmpty(ch.Delta.ReasoningContent, ch.Delta.Reasoning); rt != "" {
			if ttft == 0 {
				ttft = time.Since(start)
			}
			reasoning.WriteString(rt)
			if onChunk != nil {
				if err := onChunk(Chunk{Reasoning: rt}); err != nil {
					return nil, err
				}
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			if ttft == 0 {
				ttft = time.Since(start)
			}
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			slot, ok := toolFrags[idx]
			if !ok {
				slot = &ToolCall{Type: "function"}
				toolFrags[idx] = slot
			}
			if tc.ID != "" {
				slot.ID = tc.ID
			}
			if tc.Type != "" {
				slot.Type = tc.Type
			}
			// Gemini streams the thought_signature here, usually on the same
			// delta as the function name. Keep the first non-empty one seen
			// for this index; it must be echoed back or the next turn 400s.
			if len(tc.ExtraContent) > 0 && string(tc.ExtraContent) != "null" && len(slot.ExtraContent) == 0 {
				slot.ExtraContent = tc.ExtraContent
			}
			if tc.Function.Name != "" {
				slot.Function.Name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				slot.Function.Arguments += tc.Function.Arguments
			}
			if onChunk != nil {
				if err := onChunk(Chunk{
					ToolCallIndex: idx,
					ToolCallID:    slot.ID,
					ToolCallDelta: &FunctionCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
				}); err != nil {
					return nil, err
				}
			}
		}
	}

	// Release anything the splitter was holding back at a chunk boundary.
	if body, thought := think.flush(); body != "" || thought != "" {
		content.WriteString(body)
		reasoning.WriteString(thought)
		if onChunk != nil && body != "" {
			if err := onChunk(Chunk{Content: body}); err != nil {
				return nil, err
			}
		}
	}

	out := &Response{
		Content:      content.String(),
		Reasoning:    reasoning.String(),
		ToolCalls:    JoinToolArgs(toolFrags),
		FinishReason: finish,
		Model:        firstNonEmpty(gotModel, model),
		Provider:     c.name,
		Latency:      time.Since(start),
		TTFT:         ttft,
		Streamed:     true,
	}
	if usage != nil {
		out.Usage = *usage
	} else {
		out.Usage = EstimateUsage(req, out.Content)
	}
	return out, nil
}

// readSSELine reads one line, tolerating both \n and \r\n terminators.
func readSSELine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil {
		if err == io.EOF && line != "" {
			return line, nil
		}
		return line, err
	}
	return line, nil
}

// ---------- models ----------

func (c *OpenAICompat) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, c.wrapErr("", nil, "", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, c.wrapErr("", resp, string(raw), nil)
	}
	var lm struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &lm); err != nil {
		return nil, fmt.Errorf("%s: parse models: %w", c.name, err)
	}
	out := make([]string, 0, len(lm.Data)+len(lm.Models))
	for _, m := range lm.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	for _, m := range lm.Models { // Ollama's native shape, harmless elsewhere
		if m.Name != "" {
			out = append(out, m.Name)
		}
	}
	return out, nil
}

// ---------- helpers ----------

// flexContent accepts either a plain string or the multipart array form some
// gateways return, so one decoder handles every provider.
type flexContent string

func (f *flexContent) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	switch b[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexContent(s)
	case '[':
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(b, &parts); err != nil {
			return err
		}
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		*f = flexContent(sb.String())
	default:
		*f = "" // unknown shape; not worth failing the whole response over
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
