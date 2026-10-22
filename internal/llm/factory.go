package llm

import (
	"context"
	"fmt"

	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/ai/anthropic"
	"github.com/sky-walkerx/trequila/internal/ai/gemini"
	"github.com/sky-walkerx/trequila/internal/ai/openai"
)

// NewProvider builds the correct LLMProvider adapter based on the resolved config.
func NewProvider(ctx context.Context, cfg ai.ProviderConfig) (ai.LLMProvider, error) {
	switch cfg.Kind {
	case ai.ProviderGemini:
		return gemini.NewProvider(ctx, cfg)
	case ai.ProviderAnthropic:
		return anthropic.NewProvider(ctx, cfg)
	case ai.ProviderOpenAICompatible:
		return openai.NewProvider(ctx, cfg)
	default:
		return nil, fmt.Errorf("%w: %q", ai.ErrUnknownProvider, cfg.Kind)
	}
}
