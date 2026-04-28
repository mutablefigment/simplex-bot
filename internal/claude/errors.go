package claude

import "errors"

var (
	ErrAuth      = errors.New("claude: auth failure")
	ErrRateLimit = errors.New("claude: rate limit")
	ErrTimeout   = errors.New("claude: timeout")
	ErrCrash     = errors.New("claude: crash")
)
