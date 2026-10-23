package anthropic

import (
	"context"
	"testing"

	"github.com/sky-walkerx/trequila/internal/ai"
)

// A user can save any base_url against any provider, so a native adapter that honoured it
// would be a server-side request forgery primitive. It must always talk to Anthropic.
func TestNewProviderIgnoresUserSuppliedBaseURL(t *testing.T) {
	p, err := NewProvider(context.Background(), ai.ProviderConfig{
		Kind:    ai.ProviderAnthropic,
		APIKey:  "k",
		BaseURL: "http://169.254.169.254/latest",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", p.baseURL, defaultBaseURL)
	}
}
