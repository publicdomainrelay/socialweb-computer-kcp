//go:build js && wasm

package worker

import (
	"context"
	"errors"
	"fmt"
	"syscall/js"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
)

const (
	Kind = "webworker"

	DefaultWait = 2 * time.Minute
)

var _ sandbox.Runner = (*Runner)(nil)

type Runner struct{}

func New() *Runner { return &Runner{} }

// Fenced is false: JavaScript in a Worker is not a security boundary against the
// page's own origin, so callers that run untrusted code must refuse this runner.
func (r *Runner) Fenced() bool { return false }

func (r *Runner) Available() bool {
	return js.Global().Get("Worker").Truthy() && js.Global().Get("Blob").Truthy() && js.Global().Get("URL").Truthy()
}

func (r *Runner) Kind() string { return Kind }

const shim = `self.onmessage = async (event) => {
  const out = [];
  const errs = [];
  const render = (args) => Array.from(args).map((v) => {
    if (typeof v === "string") return v;
    try { return JSON.stringify(v); } catch (_) { return String(v); }
  }).join(" ");
  console.log = (...a) => out.push(render(a));
  console.info = console.log;
  console.debug = console.log;
  console.warn = console.log;
  console.error = (...a) => errs.push(render(a));
  let code = 0;
  try {
    const fn = new Function("self", "return (async () => {\n" + event.data + "\n})();");
    await fn(self);
  } catch (err) {
    errs.push(String((err && err.stack) || err));
    code = 1;
  }
  self.postMessage({ exitCode: code, stdout: out.join("\n"), stderr: errs.join("\n") });
};`

func (r *Runner) Run(ctx context.Context, p sandbox.Program) (result sandbox.Result, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			result = sandbox.Result{ExitCode: -1}
			err = fmt.Errorf("sandbox: the worker runner failed: %v", rec)
		}
	}()
	if !r.Available() {
		return sandbox.Result{}, errors.New("sandbox: this runtime has no Worker, Blob or URL")
	}
	if p.Source == "" {
		return sandbox.Result{}, errors.New("sandbox: the program is empty")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultWait
	}
	blob := js.Global().Get("Blob").New(
		[]any{js.ValueOf(shim)},
		map[string]any{"type": "text/javascript"},
	)
	url := js.Global().Get("URL").Call("createObjectURL", blob)
	defer js.Global().Get("URL").Call("revokeObjectURL", url)
	w := js.Global().Get("Worker").New(url, map[string]any{"type": "module"})
	defer w.Call("terminate")
	done := make(chan sandbox.Result, 1)
	onMessage := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) == 0 {
			return nil
		}
		data := args[0].Get("data")
		done <- sandbox.Result{
			ExitCode: data.Get("exitCode").Int(),
			Stdout:   data.Get("stdout").String(),
			Stderr:   data.Get("stderr").String(),
		}
		return nil
	})
	defer onMessage.Release()
	onError := js.FuncOf(func(_ js.Value, args []js.Value) any {
		msg := ""
		if len(args) > 0 {
			msg = args[0].Get("message").String()
		}
		done <- sandbox.Result{ExitCode: -1, Stderr: msg}
		return nil
	})
	defer onError.Release()
	w.Call("addEventListener", "message", onMessage)
	w.Call("addEventListener", "error", onError)
	w.Call("postMessage", js.ValueOf(p.Source))
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-done:
		return result, nil
	case <-ctx.Done():
		return sandbox.Result{ExitCode: -1, TimedOut: true}, ctx.Err()
	case <-timer.C:
		return sandbox.Result{ExitCode: -1, TimedOut: true}, fmt.Errorf("sandbox: the worker outlived %s", timeout)
	}
}
