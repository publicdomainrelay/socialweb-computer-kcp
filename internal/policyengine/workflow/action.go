package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
)

const (
	virtualOutput = "pe-virtual:GITHUB_OUTPUT"
	virtualEnv    = "pe-virtual:GITHUB_ENV"
	virtualPath   = "pe-virtual:GITHUB_PATH"
	virtualState  = "pe-virtual:GITHUB_STATE"
	virtualCache  = "pe-virtual:GITHUB_CACHE"
	virtualEvent  = "pe-virtual:GITHUB_EVENT"
	resultMarker  = "__PE_RESULT__"
)

type actionRunResult struct {
	Code   int
	VFS    map[string]string
	Stdout string
	Stderr string
}

func (e *Executor) buildStepEnv(ctx context.Context, step *models.WorkflowStep) (map[string]string, error) {
	env := make(map[string]string)
	for key, value := range e.Context.Env {
		env[key] = fmt.Sprintf("%v", value)
	}
	for key, value := range step.Env {
		evaluated, err := e.evaluateExpression(ctx, fmt.Sprintf("%v", value))
		if err != nil {
			return nil, err
		}
		env[key] = evaluated
	}
	for key, value := range step.With {
		evaluated, err := e.evaluateExpression(ctx, fmt.Sprintf("%v", value))
		if err != nil {
			return nil, err
		}
		env["INPUT_"+strings.ToUpper(key)] = evaluated
	}
	env["GITHUB_OUTPUT"] = virtualOutput
	env["GITHUB_ENV"] = virtualEnv
	env["GITHUB_PATH"] = virtualPath
	env["GITHUB_STATE"] = virtualState
	env["GITHUB_CACHE"] = virtualCache
	env["GITHUB_EVENT_PATH"] = virtualEvent
	return env, nil
}

func (e *Executor) executeStepUses(ctx context.Context, step *models.WorkflowStep, env map[string]string) error {
	if e.opts.ResolveAction == nil {
		return fmt.Errorf("no bundled action resolver configured")
	}
	action, err := e.opts.ResolveAction(step.Uses)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(action.Using, "node") {
		if action.Using == "composite" {
			return fmt.Errorf("this evaluator runs gha-lite policies net-only and refuses a composite action")
		}
		return fmt.Errorf("unsupported action type: %s", action.Using)
	}
	for name, def := range action.Inputs {
		key := "INPUT_" + strings.ToUpper(name)
		if _, exists := env[key]; exists {
			continue
		}
		if strings.TrimSpace(def) == "" {
			continue
		}
		evaluated, err := e.evaluateExpression(ctx, def)
		if err != nil {
			return err
		}
		env[key] = evaluated
	}
	env["GITHUB_ACTION_PATH"] = action.Path
	env["GITHUB_ACTION"] = step.ID
	if env["GITHUB_ACTION"] == "" {
		env["GITHUB_ACTION"] = step.Uses
	}
	result, err := e.runAction(ctx, action, env)
	e.applyActionResult(step, result)
	if err != nil {
		return err
	}
	if result.Code != 0 {
		return fmt.Errorf("action exited with code %d", result.Code)
	}
	return nil
}

func (e *Executor) runAction(ctx context.Context, action *Action, env map[string]string) (actionRunResult, error) {
	envJSON, err := json.Marshal(env)
	if err != nil {
		return actionRunResult{}, err
	}
	cacheJSON, err := json.Marshal(e.Context.Cache)
	if err != nil {
		return actionRunResult{}, err
	}
	eventJSON, err := json.Marshal(map[string]any{"inputs": e.Context.Inputs})
	if err != nil {
		return actionRunResult{}, err
	}
	cacheLiteral, _ := json.Marshal(string(cacheJSON))
	eventLiteral, _ := json.Marshal(string(eventJSON))
	shim := strings.ReplaceAll(actionShim, "__PE_ENV_JSON__", string(envJSON))
	shim = strings.ReplaceAll(shim, "__PE_CACHE_JSON__", string(cacheLiteral))
	shim = strings.ReplaceAll(shim, "__PE_EVENT_JSON__", string(eventLiteral))
	shim = strings.ReplaceAll(shim, "__PE_RESULT_MARKER__", fmt.Sprintf("%q", resultMarker))
	script := shim + "\n" + string(action.Source) + "\n" + actionFooter
	stdout, stderr, runErr := e.runDeno(ctx, script, env)
	result := parseActionResult(stdout)
	result.Stdout = stdout
	result.Stderr = stderr
	if runErr != nil && (result.VFS == nil || result.Code == 0) {
		return result, fmt.Errorf("action execution failed: %w: %s", runErr, tail(stderr, 2000))
	}
	return result, nil
}

func parseActionResult(stdout string) actionRunResult {
	result := actionRunResult{}
	found := false
	for line := range strings.SplitSeq(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, resultMarker) {
			continue
		}
		var decoded struct {
			Code int               `json:"code"`
			VFS  map[string]string `json:"vfs"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, resultMarker)), &decoded); err != nil {
			continue
		}
		result.Code = decoded.Code
		result.VFS = decoded.VFS
		found = true
	}
	if !found {
		result.Code = 1
	}
	return result
}

func (e *Executor) applyActionResult(step *models.WorkflowStep, result actionRunResult) {
	for line := range strings.SplitSeq(result.Stderr, "\n") {
		if line == "" {
			continue
		}
		e.emit(line)
		e.parseAnnotations(line)
	}
	for line := range strings.SplitSeq(result.Stdout, "\n") {
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), resultMarker) {
			continue
		}
		e.emit(line)
		e.parseAnnotations(line)
	}
	output := result.VFS[virtualOutput]
	envUpdates := result.VFS[virtualEnv]
	cacheUpdate := result.VFS[virtualCache]
	if step.ID != "" {
		outputs := parseGitHubActionsOutputs(output)
		if e.Context.Outputs[step.ID] == nil {
			e.Context.Outputs[step.ID] = map[string]any{}
		}
		e.Context.Outputs[step.ID]["outputs"] = outputs
	}
	if envUpdates != "" {
		for key, value := range parseGitHubActionsOutputs(envUpdates) {
			e.Context.Env[key] = value
		}
	}
	if cacheUpdate != "" {
		e.Context.Cache = parseCacheContent(cacheUpdate, e.Context.Cache)
	}
}

func parseCacheContent(content string, previous models.Cache) models.Cache {
	if strings.TrimSpace(content) == "" {
		return previous
	}
	var parsed models.Cache
	if err := json.Unmarshal([]byte(content), &parsed); err != nil || parsed == nil {
		return previous
	}
	return parsed
}

func tail(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[len(value)-limit:]
}

const actionShim = `const __pe_env = __PE_ENV_JSON__;
const __pe_vfs = {};
__pe_vfs[__pe_env.GITHUB_CACHE] = __PE_CACHE_JSON__;
__pe_vfs[__pe_env.GITHUB_EVENT_PATH] = __PE_EVENT_JSON__;
const __pe_ghPaths = new Set([
__pe_env.GITHUB_OUTPUT,
__pe_env.GITHUB_ENV,
__pe_env.GITHUB_PATH,
__pe_env.GITHUB_STATE,
__pe_env.GITHUB_CACHE,
__pe_env.GITHUB_EVENT_PATH,
].filter((value) => typeof value === "string" && value.length > 0));
const __pe_realExit = Deno.exit.bind(Deno);
const __pe_encoder = new TextEncoder();
function __pe_write(fd, text) {
  const bytes = __pe_encoder.encode(text);
  if (fd === 1) Deno.stdout.writeSync(bytes);
  else Deno.stderr.writeSync(bytes);
}
function __pe_finish(code) {
  __pe_write(1, __PE_RESULT_MARKER__ + JSON.stringify({ code: code, vfs: __pe_vfs }) + "\n");
}
Object.defineProperty(Deno, "env", {
  configurable: true,
  value: {
    get: (key) => __pe_env[key],
    has: (key) => Object.prototype.hasOwnProperty.call(__pe_env, key),
    set: () => {},
    delete: () => {},
    toObject: () => ({ ...__pe_env }),
  },
});
const __pe_originalWriteTextFile = Deno.writeTextFile.bind(Deno);
const __pe_originalWriteTextFileSync = Deno.writeTextFileSync.bind(Deno);
const __pe_originalReadTextFile = Deno.readTextFile.bind(Deno);
const __pe_originalReadTextFileSync = Deno.readTextFileSync.bind(Deno);
function __pe_append(key, data, options) {
  __pe_vfs[key] = (options && options.append ? (__pe_vfs[key] || "") : "") + data;
}
Deno.writeTextFile = (path, data, options) => {
  const key = String(path);
  if (__pe_ghPaths.has(key)) {
    __pe_append(key, data, options);
    return Promise.resolve();
  }
  return __pe_originalWriteTextFile(path, data, options);
};
Deno.writeTextFileSync = (path, data, options) => {
  const key = String(path);
  if (__pe_ghPaths.has(key)) {
    __pe_append(key, data, options);
    return;
  }
  return __pe_originalWriteTextFileSync(path, data, options);
};
Deno.readTextFile = (path, options) => {
  const key = String(path);
  if (__pe_ghPaths.has(key)) return Promise.resolve(__pe_vfs[key] || "");
  return __pe_originalReadTextFile(path, options);
};
Deno.readTextFileSync = (path) => {
  const key = String(path);
  if (__pe_ghPaths.has(key)) return __pe_vfs[key] || "";
  return __pe_originalReadTextFileSync(path);
};
for (const method of ["log", "error", "warn", "info", "debug", "trace"]) {
  console[method] = (...args) => {
    const line = args.map((value) => typeof value === "string" ? value : Deno.inspect(value)).join(" ");
    __pe_write(2, line + "\n");
  };
}
class __PeExit extends Error {
  constructor(code) {
    super("policy action exited");
    this.code = typeof code === "number" ? code : 0;
  }
}
Deno.exit = (code = 0) => {
  throw new __PeExit(code);
};
globalThis.addEventListener("unhandledrejection", (event) => {
  event.preventDefault();
  const reason = event.reason;
  if (!(reason instanceof __PeExit)) {
    __pe_write(2, (reason && reason.message ? reason.message : String(reason)) + "\n");
  }
  __pe_finish(reason instanceof __PeExit ? reason.code : 1);
  __pe_realExit(0);
});
globalThis.addEventListener("error", (event) => {
  event.preventDefault();
  __pe_write(2, String(event.message) + "\n");
  __pe_finish(1);
  __pe_realExit(0);
});
`

const actionFooter = `
__pe_finish(0);
__pe_realExit(0);
`
