package xrpc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const errorDetailLimit = 300

type Error struct {
	Status int

	Name string

	Message string

	Body []byte

	Wait time.Duration

	verdict Verdict
}

func (e *Error) RetryAfter() time.Duration { return e.Wait }

func retryAfter(status int, h http.Header, now time.Time) time.Duration {
	if raw := strings.TrimSpace(h.Get("Retry-After")); raw != "" {
		if s, err := strconv.ParseInt(raw, 10, 64); err == nil && s > 0 {
			return time.Duration(s) * time.Second
		}
		if at, err := http.ParseTime(raw); err == nil && at.After(now) {
			return at.Sub(now)
		}
	}
	if raw := strings.TrimSpace(h.Get("Ratelimit-Reset")); raw != "" && status == http.StatusTooManyRequests {
		if s, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if at := time.Unix(s, 0); at.After(now) {
				return at.Sub(now)
			}
		}
	}
	return 0
}

func (e *Error) HTTPStatus() int { return e.Status }

func newError(status int, body []byte) *Error {
	out := &Error{Status: status, Body: body}
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &payload) == nil {
		out.Name = strings.TrimSpace(payload.Error)
		out.Message = strings.TrimSpace(payload.Message)
	}
	return out
}

func (e *Error) Detail() string {
	switch {
	case e.Name != "" && e.Message != "":
		return e.Name + ": " + e.Message
	case e.Name != "":
		return e.Name
	case e.Message != "":
		return e.Message
	}
	text := strings.TrimSpace(string(e.Body))
	if len(text) > errorDetailLimit {
		text = text[:errorDetailLimit] + "..."
	}
	return text
}

func (e *Error) Error() string {
	if detail := e.Detail(); detail != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, detail)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}
