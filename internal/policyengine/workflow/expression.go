package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
)

var (
	expressionPattern = regexp.MustCompile(`\$\{\{\s*([\s\S]+?)\s*\}\}`)
	alwaysPattern     = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])always\(\)`)
	failurePattern    = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])failure\(\)`)
)

type guardResult struct {
	run     bool
	always  bool
	failure bool
}

func (e *Executor) evaluateGuard(ctx context.Context, condition any) (guardResult, error) {
	result := guardResult{run: true}
	if condition == nil {
		return result, nil
	}
	text, isText := condition.(string)
	if isText {
		result.always = alwaysPattern.MatchString(text)
		result.failure = failurePattern.MatchString(text)
	}
	switch typed := condition.(type) {
	case bool:
		result.run = typed
	case int:
		result.run = typed != 0
	case int64:
		result.run = typed != 0
	case float64:
		result.run = typed != 0
	case string:
		trimmed := strings.ToLower(strings.TrimSpace(typed))
		switch {
		case trimmed == "":
			result.run = false
		case trimmed == "true" || trimmed == "1":
			result.run = true
		case trimmed == "false" || trimmed == "0":
			result.run = false
		default:
			expression := typed
			if !strings.Contains(expression, "${{") {
				expression = "${{ " + expression + " }}"
			}
			evaluated, err := e.evaluateExpression(ctx, expression)
			if err != nil {
				return result, err
			}
			if strings.Contains(evaluated, "${{") {
				return result, fmt.Errorf("could not evaluate expression %q", typed)
			}
			evaluated = strings.ToLower(strings.TrimSpace(evaluated))
			if evaluated == "__github_actions_always__" {
				result.run = true
			} else if evaluated == "false" || evaluated == "0" || evaluated == "" || evaluated == "null" || evaluated == "undefined" {
				result.run = false
			} else {
				result.run = true
			}
		}
	}
	if e.jobFailed {
		result.run = result.always || result.failure
	} else if result.failure {
		result.run = false
	}
	return result, nil
}

func (e *Executor) evaluateExpression(ctx context.Context, expression string) (string, error) {
	if !strings.Contains(expression, "${{") {
		return expression, nil
	}
	matches := expressionPattern.FindAllStringSubmatchIndex(expression, -1)
	if len(matches) == 0 {
		return expression, nil
	}
	var builder strings.Builder
	last := 0
	for _, match := range matches {
		builder.WriteString(expression[last:match[0]])
		inner := strings.TrimSpace(expression[match[2]:match[3]])
		value, err := e.evaluateInnerExpression(ctx, inner)
		if err != nil {
			return "", err
		}
		builder.WriteString(value)
		last = match[1]
	}
	builder.WriteString(expression[last:])
	return builder.String(), nil
}

func (e *Executor) evaluateInnerExpression(ctx context.Context, inner string) (string, error) {
	value, err := e.evaluateUsingJavaScript(ctx, inner)
	if err == nil {
		return value, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	data := e.expressionData()
	if resolved := resolvePropertyPath(inner, data); resolved != nil {
		return fmt.Sprintf("%v", resolved), nil
	}
	return "", fmt.Errorf("could not evaluate expression %q: %w", inner, err)
}

func (e *Executor) expressionData() map[string]any {
	return map[string]any{
		"github":  e.buildGitHubContext(),
		"steps":   e.Context.Outputs,
		"inputs":  e.Context.Inputs,
		"env":     e.Context.Env,
		"secrets": e.Context.Secrets,
		"vars":    e.Context.Vars,
		"needs":   e.Context.JobResults,
		"matrix":  map[string]any{},
	}
}

func (e *Executor) evaluateUsingJavaScript(ctx context.Context, inner string) (string, error) {
	githubContext, err := json.Marshal(e.buildGitHubContext())
	if err != nil {
		return "", err
	}
	steps, err := json.Marshal(e.Context.Outputs)
	if err != nil {
		return "", err
	}
	inputs, err := json.Marshal(e.Context.Inputs)
	if err != nil {
		return "", err
	}
	env, err := json.Marshal(e.Context.Env)
	if err != nil {
		return "", err
	}
	secrets, err := json.Marshal(e.Context.Secrets)
	if err != nil {
		return "", err
	}
	vars, err := json.Marshal(e.Context.Vars)
	if err != nil {
		return "", err
	}
	needs, err := json.Marshal(e.Context.JobResults)
	if err != nil {
		return "", err
	}
	matrix, err := json.Marshal(map[string]any{})
	if err != nil {
		return "", err
	}
	failed := "false"
	if e.jobFailed {
		failed = "true"
	}
	script := `console.log(String((() => {
function always() { return "__GITHUB_ACTIONS_ALWAYS__"; }
function success() { return !__pe_failed; }
function failure() { return __pe_failed; }
function cancelled() { return false; }
function fromJSON(value) { return JSON.parse(value); }
function toJSON(value) { return JSON.stringify(value); }
function contains(haystack, needle) {
  if (Array.isArray(haystack)) return haystack.includes(needle);
  if (typeof haystack === "string") return haystack.includes(needle);
  return false;
}
function startsWith(value, prefix) { return typeof value === "string" && value.startsWith(prefix); }
function endsWith(value, suffix) { return typeof value === "string" && value.endsWith(suffix); }
function format(value) {
  const args = Array.prototype.slice.call(arguments, 1);
  return String(value).replace(/\{(\d+)\}/g, (match, index) => args[Number(index)] !== undefined ? String(args[Number(index)]) : match);
}
function join(value, separator) { return Array.isArray(value) ? value.join(separator === undefined ? "," : separator) : String(value); }
function hashFiles() { return ""; }
const __pe_failed = ` + failed + `;
const github = ` + string(githubContext) + `;
const runner = {"debug":1};
const steps = ` + string(steps) + `;
const inputs = ` + string(inputs) + `;
const env = ` + string(env) + `;
const secrets = ` + string(secrets) + `;
const vars = ` + string(vars) + `;
const needs = ` + string(needs) + `;
const matrix = ` + string(matrix) + `;
return (` + transformPropertyAccessors(inner) + `);
})()))
`
	stdout, stderr, err := e.runDeno(ctx, script, nil)
	if err != nil {
		return "", fmt.Errorf("deno evaluation failed: %w: %s", err, strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(stdout), nil
}

func (e *Executor) buildGitHubContext() map[string]any {
	context := make(map[string]any)
	for key, value := range e.Context.Env {
		if !strings.HasPrefix(key, "GITHUB_") {
			continue
		}
		name := strings.Replace(strings.ToLower(key), "github_", "", 1)
		context[name] = value
	}
	if _, exists := context["token"]; !exists {
		context["token"] = e.Context.Secrets["GITHUB_TOKEN"]
	}
	context["event"] = map[string]any{"inputs": e.Context.Inputs}
	return context
}

func resolvePropertyPath(path string, data map[string]any) any {
	var current any = data
	for part := range strings.SplitSeq(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		value, exists := object[part]
		if !exists {
			return nil
		}
		current = value
	}
	return current
}

func transformPropertyAccessors(code string) string {
	var builder strings.Builder
	index := 0
	for index < len(code) {
		character := code[index]
		if character == '"' || character == '\'' {
			quote := character
			builder.WriteByte(character)
			index++
			for index < len(code) {
				builder.WriteByte(code[index])
				if code[index] == quote {
					index++
					break
				}
				index++
			}
			continue
		}
		if character == '.' {
			builder.WriteString("['")
			index++
			start := index
			for index < len(code) && isPropertyByte(code[index]) {
				index++
			}
			builder.WriteString(code[start:index])
			builder.WriteString("']")
			continue
		}
		builder.WriteByte(character)
		index++
	}
	return builder.String()
}

func isPropertyByte(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || value == '_' || value == '-'
}

func (e *Executor) runDeno(ctx context.Context, script string, extraEnv map[string]string) (string, string, error) {
	if e.opts.Sandbox == nil || !e.opts.Sandbox.Available() {
		return "", "", fmt.Errorf("deno runtime unavailable")
	}
	if !e.opts.Sandbox.Fenced() {
		return "", "", fmt.Errorf("the %s sandbox runner enforces no fence; policy code is not run there", e.opts.Sandbox.Kind())
	}
	env := make(map[string]string, len(extraEnv)+2)
	maps.Copy(env, extraEnv)
	if _, exists := env["NO_COLOR"]; !exists {
		env["NO_COLOR"] = "1"
	}
	if _, exists := env["DENO_NO_UPDATE_CHECK"]; !exists {
		env["DENO_NO_UPDATE_CHECK"] = "1"
	}
	result, err := e.opts.Sandbox.Run(ctx, sandbox.Program{Source: script, Net: true, Env: env})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result.Stdout, result.Stderr, ctxErr
	}
	if err != nil {
		return result.Stdout, result.Stderr, err
	}
	if result.TimedOut {
		return result.Stdout, result.Stderr, fmt.Errorf("the policy sandbox outlived its deadline")
	}
	if result.ExitCode != 0 {
		return result.Stdout, result.Stderr, fmt.Errorf("policy code exited with %d", result.ExitCode)
	}
	return result.Stdout, result.Stderr, nil
}
