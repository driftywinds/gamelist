package api

import "fmt"

// APIError represents a common error structure for API responses.
type APIError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API Error %d: %s", e.Code, e.Message)
}
