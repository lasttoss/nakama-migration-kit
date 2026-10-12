package playfab

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// APIError is an error the Admin API reported, with the code the title sent.
type APIError struct {
	StatusCode int
	Code       int
	Name       string
	Message    string
	RetryAfter string
	Method     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("playfab: %s: HTTP %d code %d %s: %s", e.Method, e.StatusCode, e.Code, e.Name, e.Message)
}

// RateLimited reports whether the title asked us to slow down.
func (e *APIError) RateLimited() bool {
	return e.Code == http.StatusTooManyRequests || e.Name == "RateLimitExceeded"
}

// ServerError reports whether the title failed rather than refused.
func (e *APIError) ServerError() bool {
	return e.Code >= 500 || e.StatusCode >= 500
}

// Retryable reports whether another attempt can succeed. A 429 is the normal case on a busy
// title; a 5xx is worth another try; anything else in the 4xx range is a mistake in the request
// and retrying it spends wall clock that the rate limit is already spending.
func (e *APIError) Retryable() bool {
	if e.RateLimited() || e.ServerError() {
		return true
	}
	return e.Code == http.StatusRequestTimeout || e.Code == http.StatusConflict
}

// IsNotFound reports whether the player simply is not in the title, which an import treats as a
// skipped player rather than a failed one.
func (e *APIError) IsNotFound() bool {
	return e.Code == http.StatusNotFound || strings.EqualFold(e.Name, "PlayerNotFound")
}

// asAPIError unwraps an APIError from an error chain.
func asAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// IsNotFound reports whether err says the player is not in the title.
func IsNotFound(err error) bool {
	apiErr, ok := asAPIError(err)
	return ok && apiErr.IsNotFound()
}
