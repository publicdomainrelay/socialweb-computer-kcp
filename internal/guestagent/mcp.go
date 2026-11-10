package guestagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

const (
	MCPSocketFile = "mcp.sock"

	MCPPath = "/mcp"

	MCPServerName = "socialweb-guest-agent"

	mcpProtocolLatest = "2025-11-25"

	mcpMaxBody = 8 << 20

	mcpMaxLine = 16 << 20
)

var mcpProtocols = []string{mcpProtocolLatest, "2025-06-18", "2025-03-26"}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`

	ID json.RawMessage `json:"id,omitempty"`

	Method string `json:"method"`

	Params json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code int `json:"code"`

	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string `json:"jsonrpc"`

	ID json.RawMessage `json:"id"`

	Result any `json:"result,omitempty"`

	Error *rpcError `json:"error,omitempty"`
}

type mcpTool struct {
	Name string `json:"name"`

	Description string `json:"description"`

	InputSchema json.RawMessage `json:"inputSchema"`
}

var mcpTools = []mcpTool{
	{"exec", "Run a command with /bin/sh -c as root and wait for it. stdout and stderr are each capped at 256 KiB.",
		json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"stdin":{"type":"string"},"timeoutMs":{"type":"integer","minimum":1,"maximum":600000,"default":60000},"cwd":{"type":"string"}},"required":["command"]}`)},
	{"start", "Start a background job with /bin/sh -c as root. Follow it with read (waitMs long-polls); its stdin stays open for write.",
		json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"cwd":{"type":"string"},"stdin":{"type":"string"}},"required":["command"]}`)},
	{"read", "Read a job's interleaved stdout and stderr from a byte cursor. done is true once the job exited and the cursor reached the end.",
		json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"cursor":{"type":"integer","minimum":0,"default":0},"maxBytes":{"type":"integer","minimum":1,"maximum":262144,"default":65536},"waitMs":{"type":"integer","minimum":0,"maximum":30000}},"required":["id"]}`)},
	{"write", "Write to a job's stdin; close closes it.",
		json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"data":{"type":"string"},"close":{"type":"boolean"}},"required":["id","data"]}`)},
	{"kill", "Signal a job's process group.",
		json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"signal":{"type":"string","enum":["TERM","KILL","INT"],"default":"TERM"}},"required":["id"]}`)},
	{"list", "List the jobs.", json.RawMessage(`{"type":"object","properties":{}}`)},
}

type mcpServer struct {
	jobs *jobTable

	log *slog.Logger
}

func newMCPServer(log *slog.Logger) *mcpServer {
	return &mcpServer{jobs: newJobTable(), log: log}
}

func (m *mcpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != MCPPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only; this server opens no SSE stream", http.StatusMethodNotAllowed)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(mcpProtocols, v) {
		http.Error(w, "unsupported MCP-Protocol-Version "+v, http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpMaxBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	reply, status := m.handle(r.Context(), body)
	if reply == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(reply)
}

func (m *mcpServer) handle(ctx context.Context, body []byte) ([]byte, int) {
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" {
		return mustJSON(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "not a JSON-RPC 2.0 message"}}), http.StatusBadRequest
	}
	if len(req.ID) == 0 || req.Method == "" {
		return nil, 0
	}
	result, rerr := m.call(ctx, req.Method, req.Params)
	return mustJSON(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}), http.StatusOK
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		raw, _ = json.Marshal(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32603, err.Error()}})
	}
	return raw
}

func (m *mcpServer) call(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &p)
		version := mcpProtocolLatest
		if slices.Contains(mcpProtocols, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": MCPServerName, "version": "1"},
			"instructions":    "A root shell in this VM. exec runs a command and waits; start, read (waitMs long-polls), write, kill and list manage background jobs that outlive this connection.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name string `json:"name"`

			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{-32602, "tools/call params: " + err.Error()}
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage("{}")
		}
		out, err := m.tool(ctx, p.Name, p.Arguments)
		if errors.Is(err, errUnknownTool) {
			return nil, &rpcError{-32602, err.Error()}
		}
		if err != nil {
			return map[string]any{"content": []map[string]string{{"type": "text", "text": err.Error()}}, "isError": true}, nil
		}
		text, _ := json.Marshal(out)
		return map[string]any{"content": []map[string]string{{"type": "text", "text": string(text)}}, "structuredContent": out}, nil
	}
	return nil, &rpcError{-32601, "method not found: " + method}
}

var errUnknownTool = errors.New("unknown tool")

func (m *mcpServer) tool(ctx context.Context, name string, args json.RawMessage) (any, error) {
	var a struct {
		Command string `json:"command"`

		Stdin string `json:"stdin"`

		Cwd string `json:"cwd"`

		TimeoutMs int64 `json:"timeoutMs"`

		ID string `json:"id"`

		Cursor int64 `json:"cursor"`

		MaxBytes int `json:"maxBytes"`

		WaitMs int64 `json:"waitMs"`

		Data *string `json:"data"`

		Close bool `json:"close"`

		Signal string `json:"signal"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("the arguments of %s: %w", name, err)
	}
	needCommand := func() error {
		if a.Command == "" {
			return errors.New("command is required")
		}
		return nil
	}
	switch name {
	case "exec":
		if err := needCommand(); err != nil {
			return nil, err
		}
		m.log.Info("mcp exec", "commandBytes", len(a.Command))
		return runExec(ctx, a.Command, a.Stdin, a.Cwd, time.Duration(a.TimeoutMs)*time.Millisecond)
	case "start":
		if err := needCommand(); err != nil {
			return nil, err
		}
		id, err := m.jobs.start(a.Command, a.Cwd, a.Stdin)
		if err != nil {
			return nil, err
		}
		m.log.Info("mcp start", "job", id, "commandBytes", len(a.Command))
		return map[string]string{"id": id}, nil
	case "list":
		return map[string]any{"jobs": m.jobs.list()}, nil
	case "read", "write", "kill":
	default:
		return nil, fmt.Errorf("%w %q", errUnknownTool, name)
	}
	j, err := m.jobs.get(a.ID)
	if err != nil {
		return nil, err
	}
	switch name {
	case "read":
		return j.read(ctx, a.Cursor, a.MaxBytes, time.Duration(a.WaitMs)*time.Millisecond), nil
	case "write":
		if a.Data == nil {
			return nil, errors.New("data is required")
		}
		return map[string]any{}, j.write(*a.Data, a.Close)
	default:
		if a.Signal == "" {
			a.Signal = "TERM"
		}
		return map[string]any{}, j.kill(a.Signal)
	}
}

func (a *Agent) mcpSocket() string { return filepath.Join(a.cfg.StateDir, MCPSocketFile) }

func (a *Agent) runMCP(ctx context.Context) {
	path := a.mcpSocket()
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		a.log.Error("the mcp socket does not listen", "path", path, "err", err.Error())
		return
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		a.log.Error("the mcp socket mode", "path", path, "err", err.Error())
		return
	}
	srv := &http.Server{Handler: newMCPServer(a.log), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	a.log.Info("the mcp server listens", "path", path)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		a.log.Error("the mcp server stopped", "err", err.Error())
	}
	_ = os.Remove(path)
}

func MCPStdio(ctx context.Context, socket string, in io.Reader, out io.Writer) error {
	client := &http.Client{Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	var mu sync.Mutex
	emit := func(line []byte) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = out.Write(append(bytes.TrimSpace(line), '\n'))
	}
	var wg sync.WaitGroup
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), mcpMaxLine)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		msg := slices.Clone(line)
		wg.Go(func() {
			reply, err := mcpPost(ctx, client, msg)
			if err != nil {
				var req rpcRequest
				if json.Unmarshal(msg, &req) == nil && len(req.ID) > 0 && req.Method != "" {
					emit(mustJSON(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{-32603, "the guest agent daemon at " + socket + ": " + err.Error()}}))
				}
				return
			}
			if len(bytes.TrimSpace(reply)) > 0 {
				emit(reply)
			}
		})
	}
	wg.Wait()
	return scanner.Err()
}

func mcpPost(ctx context.Context, client *http.Client, msg []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://guest"+MCPPath, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	switch {
	case err != nil:
		return nil, err
	case resp.StatusCode == http.StatusAccepted:
		return nil, nil
	case resp.Header.Get("Content-Type") != "application/json":
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return body, nil
}
