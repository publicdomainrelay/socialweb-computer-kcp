package guestidentity

import (
	"fmt"
	"strings"
)

type LoginStep string

const (
	LoginBind LoginStep = "bind"

	LoginChallenge LoginStep = "challenge"

	LoginLogin LoginStep = "login"
)

const maximumLoginField = 4096

var loginFields = map[LoginStep][]string{
	LoginBind:      {"role", "bind_secret", "host_key", "signature"},
	LoginChallenge: {"role"},
	LoginLogin:     {"role", "challenge", "signature"},
}

func ParseLoginStep(step string) (LoginStep, error) {
	s := LoginStep(step)
	if _, ok := loginFields[s]; !ok {
		return "", fmt.Errorf("guestidentity: %q is not a guest login step", step)
	}
	return s, nil
}

func (s LoginStep) Fields() []string {
	fields, ok := loginFields[s]
	if !ok {
		return nil
	}
	return append([]string(nil), fields...)
}

func LoginBody(step LoginStep, input map[string]any) (map[string]string, error) {
	fields, ok := loginFields[step]
	if !ok {
		return nil, fmt.Errorf("guestidentity: %q is not a guest login step", step)
	}
	out := make(map[string]string, len(fields))
	for name, value := range input {
		if !contains(fields, name) {
			return nil, fmt.Errorf("guestidentity: the %s step takes %s, not %s", step, strings.Join(fields, ", "), name)
		}
		text, ok := value.(string)
		if !ok || text == "" || len(text) > maximumLoginField {
			return nil, fmt.Errorf("guestidentity: %s must be a string of 1 to %d bytes", name, maximumLoginField)
		}
		out[name] = text
	}
	for _, name := range fields {
		if out[name] == "" {
			return nil, fmt.Errorf("guestidentity: the %s step needs %s", step, name)
		}
	}
	role := out["role"]
	if !ValidComputeVM(role) {
		return nil, fmt.Errorf("guestidentity: role %q is not a ComputeVM name", role)
	}
	return out, nil
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
