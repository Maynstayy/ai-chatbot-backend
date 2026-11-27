package chatbot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// maxToolRounds caps model <-> tool round trips per user message, and
// maxCallsPerRound the tool calls run from one model turn, so a confused or
// manipulated model can't loop over the database.
const (
	maxToolRounds    = 4
	maxCallsPerRound = 5
)

// anthropicProvider calls Claude through the official Go SDK
// (github.com/anthropics/anthropic-sdk-go). It runs the tool-use loop itself
// and keeps the SDK's own message history for the length of one request, so
// whatever the model returns (thinking blocks included) is sent back unchanged.
type anthropicProvider struct {
	client    anthropic.Client
	model     string
	maxTokens int64
	fallbacks bool
}

// extra options are for tests (e.g. option.WithBaseURL to a fake server).
func newAnthropicProvider(apiKey, model string, maxTokens int, timeout time.Duration, extra ...option.RequestOption) *anthropicProvider {
	opts := append([]option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithRequestTimeout(timeout),
		option.WithMaxRetries(1),
	}, extra...)
	return &anthropicProvider{
		client:    anthropic.NewClient(opts...),
		model:     model,
		maxTokens: int64(maxTokens),
		fallbacks: supportsServerFallback(model),
	}
}

// supportsServerFallback reports whether the model takes the server-side
// refusal fallback ("fallbacks": "default"), which re-serves a declined
// request from another model inside the same call.
func supportsServerFallback(model string) bool {
	for _, p := range []string{"claude-opus-5", "claude-fable-5", "claude-sonnet-5-5"} {
		if strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}

func (p *anthropicProvider) Reply(ctx context.Context, req ProviderRequest) (string, error) {
	tools := make([]anthropic.BetaToolUnionParam, 0, len(req.Tools))
	for _, t := range req.Tools {
		tool := anthropic.BetaToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.BetaToolInputSchemaParam{
				Properties:  t.Properties,
				Required:    t.Required,
				ExtraFields: map[string]any{"additionalProperties": false},
			},
		}
		tools = append(tools, anthropic.BetaToolUnionParam{OfTool: &tool})
	}

	messages := make([]anthropic.BetaMessageParam, 0, len(req.History)+2*maxToolRounds)
	for _, t := range req.History {
		if t.Role == "assistant" {
			messages = append(messages, anthropic.BetaMessageParam{OfAssistant: &anthropic.BetaAssistantMessageParam{
				Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(t.Text)},
			}})
		} else {
			messages = append(messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(t.Text)))
		}
	}

	params := anthropic.BetaMessageNewParams{
		Model:     p.model,
		MaxTokens: p.maxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: req.System}},
		Tools:     tools,
		// Chat answers don't need deep reasoning; low effort keeps latency
		// and cost down.
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortLow},
	}
	if p.fallbacks {
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
	}

	for round := 0; ; round++ {
		params.Messages = messages
		resp, err := p.client.Beta.Messages.New(ctx, params)
		if err != nil {
			return "", fmt.Errorf("anthropic messages: %w", err)
		}
		if resp.StopReason == anthropic.BetaStopReasonRefusal {
			return "", ErrProviderRefused
		}
		messages = append(messages, resp.ToParam())

		var text strings.Builder
		var results []anthropic.BetaContentBlockParamUnion
		calls := 0
		for _, block := range resp.Content {
			switch b := block.AsAny().(type) {
			case anthropic.BetaTextBlock:
				text.WriteString(b.Text)
			case anthropic.BetaToolUseBlock:
				calls++
				if calls > maxCallsPerRound || round >= maxToolRounds {
					results = append(results, anthropic.NewBetaToolResultBlock(b.ID,
						`{"error":"tool call limit reached; answer with the information you already have"}`, true))
					continue
				}
				out, isErr := req.Execute(ctx, ToolCall{Name: b.Name, Input: []byte(b.JSON.Input.Raw())})
				results = append(results, anthropic.NewBetaToolResultBlock(b.ID, out, isErr))
			}
		}

		if resp.StopReason != anthropic.BetaStopReasonToolUse || len(results) == 0 {
			if s := strings.TrimSpace(text.String()); s != "" {
				return s, nil
			}
			return "", ErrProviderEmpty
		}
		if round > maxToolRounds {
			return "", fmt.Errorf("anthropic: tool loop did not finish")
		}
		messages = append(messages, anthropic.NewBetaUserMessage(results...))
	}
}
