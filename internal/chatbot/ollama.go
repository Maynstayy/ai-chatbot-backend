package chatbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ollamaProvider calls a local Ollama server through its native chat API
// (POST /api/chat, non-streaming). It runs the same tool-use loop as the
// Claude provider - same round and per-round limits - and every tool call
// goes through ProviderRequest.Execute, so the tools, their validation and
// booking ownership checks are shared. Ollama needs no API key.
type ollamaProvider struct {
	client    *http.Client
	chatURL   string
	model     string
	maxTokens int
}

// ollamaContextTokens is the context window requested per call. Ollama's
// default (2-4k) is too small for the system prompt, tool schemas and a few
// tool results; qwen2.5 supports 32k.
const ollamaContextTokens = 8192

// maxOllamaResponseBytes bounds how much of a response body is read.
const maxOllamaResponseBytes = 4 << 20

func newOllamaProvider(baseURL, model string, maxTokens int, timeout time.Duration) *ollamaProvider {
	return &ollamaProvider{
		client:    &http.Client{Timeout: timeout},
		chatURL:   strings.TrimRight(baseURL, "/") + "/api/chat",
		model:     model,
		maxTokens: maxTokens,
	}
}

// --- wire types (https://github.com/ollama/ollama/blob/main/docs/api.md) ---

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaMessage struct {
	Role      string           `json:"role"` // system, user, assistant, tool
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"` // on role "tool"
}

type ollamaTool struct {
	Type     string             `json:"type"` // always "function"
	Function ollamaToolFunction `json:"function"`
}

type ollamaToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ollamaToolCall struct {
	Function ollamaCalledFunction `json:"function"`
}

type ollamaCalledFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ollamaChatResponse struct {
	Message    ollamaMessage `json:"message"`
	Done       bool          `json:"done"`
	DoneReason string        `json:"done_reason"`
	Error      string        `json:"error"`
}

func (p *ollamaProvider) Reply(ctx context.Context, req ProviderRequest) (string, error) {
	specs := make(map[string]ToolSpec, len(req.Tools))
	tools := make([]ollamaTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		specs[t.Name] = t
		required := t.Required
		if required == nil {
			required = []string{}
		}
		tools = append(tools, ollamaTool{Type: "function", Function: ollamaToolFunction{
			Name:        t.Name,
			Description: t.Description,
			Parameters: map[string]any{
				"type":       "object",
				"properties": t.Properties,
				"required":   required,
			},
		}})
	}

	messages := make([]ollamaMessage, 0, len(req.History)+1+3*maxToolRounds)
	if req.System != "" {
		messages = append(messages, ollamaMessage{Role: "system", Content: req.System + ollamaToolGuidance})
	}
	for _, t := range req.History {
		role := "user"
		if t.Role == "assistant" {
			role = "assistant"
		}
		messages = append(messages, ollamaMessage{Role: role, Content: t.Text})
	}

	for round := 0; ; round++ {
		body := ollamaChatRequest{
			Model:    p.model,
			Messages: messages,
			Stream:   false,
			Options: map[string]any{
				"num_predict": p.maxTokens,
				"num_ctx":     ollamaContextTokens,
				// Low temperature keeps tool arguments and facts stable.
				"temperature": 0.2,
			},
		}
		// On the last allowed round the tools are withdrawn, so the model
		// has to answer from the results it already has.
		if round < maxToolRounds {
			body.Tools = tools
		}

		resp, err := p.chat(ctx, body)
		if err != nil {
			return "", err
		}

		calls := resp.Message.ToolCalls
		content := resp.Message.Content
		if len(calls) == 0 && len(specs) > 0 {
			// Small models sometimes write the call as text instead of using
			// the structured field; only offered tool names are accepted.
			if c, ok := toolCallsFromText(content, specs); ok {
				calls, content = c, ""
			}
		}

		if len(calls) == 0 {
			if s := cleanOllamaText(content); s != "" {
				return s, nil
			}
			return "", ErrProviderEmpty
		}
		if round > maxToolRounds {
			return "", errors.New("ollama: tool loop did not finish")
		}

		assistant := ollamaMessage{Role: "assistant", Content: content}
		var results []ollamaMessage
		for i, c := range calls {
			name := c.Function.Name
			args, argErr := normalizeToolArgs(c.Function.Arguments, specs[name])
			if argErr != nil {
				args = json.RawMessage("{}")
			}
			assistant.ToolCalls = append(assistant.ToolCalls, ollamaToolCall{
				Function: ollamaCalledFunction{Name: name, Arguments: args},
			})

			var out string
			switch _, offered := specs[name]; {
			case !offered:
				// Never run anything that was not offered for this caller.
				out = errorResult("unknown tool " + strconv.Quote(name))
			case i >= maxCallsPerRound || round >= maxToolRounds:
				out = `{"error":"tool call limit reached; answer with the information you already have"}`
			case argErr != nil:
				out = errorResult("invalid arguments: " + argErr.Error())
			default:
				out, _ = req.Execute(ctx, ToolCall{Name: name, Input: args})
			}
			results = append(results, ollamaMessage{Role: "tool", Content: out, ToolName: name})
		}
		messages = append(messages, assistant)
		messages = append(messages, results...)
	}
}

// ollamaToolGuidance is added to the system prompt for local models, which
// follow the general rules less reliably than large hosted ones.
const ollamaToolGuidance = `

Tool use (important):
- Before you name any hotel, price, room, availability, booking or payment detail, call the matching tool and use only what it returned. If you have not called a tool, you do not know these facts.
- Call tools through the tool-calling interface, never by writing JSON in your reply.
- Use only the argument names listed in each tool's schema. Leave out arguments you do not have.
- When the guest names a city or destination, call search_hotels right away with only that city. Do not ask for a budget or category first; they are optional.
- Never make up argument values. Add min_price or max_price only when the guest wrote a budget number, and then only that number. Add a booking status only when the guest named one. Use a booking_id only when the guest typed it; otherwise ask for it.
- If "Lookups already done" results are given below, answer from them first.
- Answer the guest's latest message. Earlier messages are context only.
- Repeat numbers and statuses exactly as the tool gave them, with their meaning (rating is out of 5, total_reviews is a count). If a field is empty or missing, say it is not available - do not fill it in.
- If the guest asks about a booking and is not signed in, tell them to log in first and set needs_login="true".
- Always end with the <meta .../> line described above, as the very last line.`

// chat sends one non-streaming /api/chat request.
func (p *ollamaProvider) chat(ctx context.Context, body ollamaChatRequest) (*ollamaChatResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("ollama: encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.chatURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("ollama: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	res, err := p.client.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("ollama chat: %w", ctxErr)
		}
		return nil, fmt.Errorf("ollama chat: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxOllamaResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("ollama chat: read response: %w", err)
	}

	var out ollamaChatResponse
	jsonErr := json.Unmarshal(raw, &out)
	if res.StatusCode < 200 || res.StatusCode > 299 {
		msg := strings.TrimSpace(out.Error)
		if jsonErr != nil || msg == "" {
			msg = http.StatusText(res.StatusCode)
		}
		return nil, fmt.Errorf("ollama chat: HTTP %d: %s", res.StatusCode, msg)
	}
	if jsonErr != nil {
		return nil, fmt.Errorf("ollama chat: malformed response: %w", jsonErr)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("ollama chat: %s", out.Error)
	}
	return &out, nil
}

// normalizeToolArgs turns the model's arguments into the JSON object the tool
// executor expects. Ollama sends an object, but small models sometimes send a
// JSON string, nulls for missing values, or numbers as strings; those are
// tidied using the tool's schema. Anything else is left for the executor's
// own strict validation to accept or reject.
func normalizeToolArgs(raw json.RawMessage, spec ToolSpec) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}"), nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, errors.New("arguments are not a JSON object")
		}
		raw = bytes.TrimSpace([]byte(s))
		if len(raw) == 0 {
			return json.RawMessage("{}"), nil
		}
	}
	var args map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&args); err != nil || args == nil {
		return nil, errors.New("arguments are not a JSON object")
	}
	for k, v := range args {
		if v == nil {
			delete(args, k)
			continue
		}
		s, isStr := v.(string)
		if !isStr {
			continue
		}
		prop, _ := spec.Properties[k].(map[string]any)
		switch prop["type"] {
		case "integer", "number":
			s = strings.TrimSpace(s)
			if s == "" {
				delete(args, k)
			} else if _, err := strconv.ParseFloat(s, 64); err == nil {
				args[k] = json.Number(s)
			}
		case "string":
			if strings.TrimSpace(s) == "" {
				delete(args, k)
			}
		}
	}
	out, err := json.Marshal(args)
	if err != nil {
		return nil, errors.New("arguments are not a JSON object")
	}
	return out, nil
}

var (
	toolCallTag  = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*(?:</tool_call>|$)`)
	codeFence    = regexp.MustCompile("(?s)^```(?:json)?\\s*(.*?)\\s*```$")
	thinkSection = regexp.MustCompile(`(?s)<think>.*?</think>`)
)

// toolCallsFromText recovers tool calls a model wrote into its reply text,
// either as <tool_call>{...}</tool_call> blocks or as a reply that is nothing
// but a {"name":...,"arguments":...} object. Only offered tools count.
func toolCallsFromText(content string, specs map[string]ToolSpec) ([]ollamaToolCall, bool) {
	s := strings.TrimSpace(thinkSection.ReplaceAllString(content, ""))
	if s == "" {
		return nil, false
	}
	var candidates []string
	if m := toolCallTag.FindAllStringSubmatch(s, -1); m != nil {
		for _, x := range m {
			candidates = append(candidates, x[1])
		}
	} else {
		if m := codeFence.FindStringSubmatch(s); m != nil {
			s = m[1]
		}
		if !strings.HasPrefix(s, "{") {
			return nil, false
		}
		candidates = []string{s}
	}
	var calls []ollamaToolCall
	for _, c := range candidates {
		var fn struct {
			Name       string          `json:"name"`
			Arguments  json.RawMessage `json:"arguments"`
			Parameters json.RawMessage `json:"parameters"`
		}
		if err := json.Unmarshal([]byte(c), &fn); err != nil {
			return nil, false
		}
		if _, ok := specs[fn.Name]; !ok {
			return nil, false
		}
		args := fn.Arguments
		if len(args) == 0 {
			args = fn.Parameters
		}
		calls = append(calls, ollamaToolCall{Function: ollamaCalledFunction{Name: fn.Name, Arguments: args}})
	}
	return calls, len(calls) > 0
}

// cleanOllamaText drops reasoning sections some local models emit.
func cleanOllamaText(s string) string {
	return strings.TrimSpace(thinkSection.ReplaceAllString(s, ""))
}
