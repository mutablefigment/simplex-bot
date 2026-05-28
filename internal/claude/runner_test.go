package claude

import (
	"errors"
	"strings"
	"testing"
)

// Issue #4: stderr must never bleed into the returned error string.
// classifyStderr inspects stderr to choose a typed error but must not embed it.
func TestClassifyStderr_NoLeak(t *testing.T) {
	secret := "sk-ant-api03-abc123-DEF456-secret-leak-marker"
	base := errors.New("claude: crash")

	tests := []struct {
		name   string
		stderr string
		want   error
	}{
		{"rate_limit", "rate limit reached " + secret, ErrRateLimit},
		{"rate_limit_underscore", "Error: rate_limit exceeded — " + secret, ErrRateLimit},
		{"auth_unauthorized", "Unauthorized: " + secret, ErrAuth},
		{"auth_authentication", "Authentication failed: " + secret, ErrAuth},
		{"auth_api_key", "invalid api key " + secret, ErrAuth},
		{"unclassified", "panic in subprocess at /tmp/" + secret, base},
		{"empty", "", base},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyStderr(base, tt.stderr)
			if !errors.Is(got, tt.want) {
				t.Errorf("classifyStderr error type = %v, want %v", got, tt.want)
			}
			if strings.Contains(got.Error(), "sk-ant") || strings.Contains(got.Error(), "secret-leak-marker") {
				t.Errorf("stderr leaked into error message: %q", got.Error())
			}
		})
	}
}
