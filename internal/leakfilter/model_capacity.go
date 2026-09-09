package leakfilter

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// IsModelCapacityError recognizes the provider's model-capacity failure only
// inside an error envelope. The same words in generated assistant text must stay
// untouched. This predicate is shared by retry and downstream sanitization so a
// retryable capacity failure cannot be classified one way and rendered another.
func IsModelCapacityError(status int, body []byte) bool {
	root := gjson.ParseBytes(body)
	failure := root.Get("error")
	if !failure.Exists() || failure.Type == gjson.Null {
		failure = root.Get("response.error")
	}
	if !failure.Exists() || failure.Type == gjson.Null {
		if status < http.StatusBadRequest {
			return false
		}
		failure = root
	}
	code := strings.ToLower(strings.TrimSpace(failure.Get("code").String()))
	if code == "context_length_exceeded" || code == "invalid_request_error" {
		return false
	}
	if code == "server_is_overloaded" || code == "model_capacity_exceeded" {
		return true
	}
	message := failure.Get("message").String()
	if failure.Type == gjson.String {
		message = failure.String()
	} else if !gjson.ValidBytes(body) && status >= http.StatusBadRequest {
		message = string(body)
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(message)), "selected model is at capacity")
}
