package xrpc

import (
	"encoding/json"
	"fmt"
	"strings"
)

const errorDetailLimit = 300

type Error struct {
	Status int

	Name string

	Message string

	Body []byte

	verdict Verdict
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
