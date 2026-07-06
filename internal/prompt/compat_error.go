package prompt

import (
	"errors"
	"fmt"
	"strings"
)

// CompatibilityError marks an official-client feature that cannot be represented
// through the Chat Completions bridge without silently dropping behavior.
type CompatibilityError struct {
	Protocol string
	Kind     string
	Value    string
}

func (e *CompatibilityError) Error() string {
	value := strings.TrimSpace(e.Value)
	if value == "" {
		value = "<empty>"
	}
	protocol := strings.TrimSpace(e.Protocol)
	if protocol == "" {
		protocol = "request"
	}
	return fmt.Sprintf("unsupported %s %s %q for chat_completions bridge; configure a native provider or use an official account for this request", protocol, e.Kind, value)
}

func AsCompatibilityError(err error) (*CompatibilityError, bool) {
	var ce *CompatibilityError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}
