package guestagent

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// The process contract over plain HTTP runs the same tools as /mcp: start, write, read to the end.
func TestProcessContract(t *testing.T) {
	srv := httptest.NewServer(newMCPServer(slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer srv.Close()
	post := func(tool, body string) (int, map[string]any) {
		res, err := http.Post(srv.URL+ProcessPrefix+tool, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	// the script in the write that closes stdin: start's own stdin is written asynchronously, so a
	// close right after it could come first
	status, started := post("start", `{"command":"sh -s"}`)
	if status != 200 || started["id"] == nil {
		t.Fatalf("start: %d %v", status, started)
	}
	id := started["id"].(string)
	if status, _ := post("write", `{"id":"`+id+`","data":"echo hi; exit 3\n","close":true}`); status != 200 {
		t.Fatalf("write: %d", status)
	}
	var output string
	cursor := 0.0
	for {
		status, read := post("read", `{"id":"`+id+`","waitMs":2000,"cursor":`+strconv.Itoa(int(cursor))+`}`)
		if status != 200 {
			t.Fatalf("read: %d %v", status, read)
		}
		output += read["output"].(string)
		cursor = read["cursor"].(float64)
		if read["done"] == true {
			if read["exitCode"] != float64(3) || output != "hi\n" {
				t.Fatalf("done: %v %q", read["exitCode"], output)
			}
			break
		}
	}
	if status, out := post("nope", `{}`); status != 404 || out["error"] != "MethodNotImplemented" {
		t.Fatalf("unknown tool: %d %v", status, out)
	}
	if status, out := post("start", `{}`); status != 400 || out["message"] != "command is required" {
		t.Fatalf("no command: %d %v", status, out)
	}
	if res, _ := http.Get(srv.URL + ProcessPrefix + "list"); res.StatusCode != 405 {
		t.Fatalf("GET: %d", res.StatusCode)
	}
}
