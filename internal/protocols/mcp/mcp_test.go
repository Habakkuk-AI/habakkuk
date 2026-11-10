package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	"github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
	"github.com/habakkuk-ai/habakkuk/internal/testutil"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// The test binary doubles as a stdio MCP server when this variable is set.
const serverEnv = "HABAKKUK_MCP_TEST_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(serverEnv) != "" {
		_ = testServer().Run(context.Background(), &sdk.StdioTransport{})
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// testServer offers a few tools, two per tools/list page so the client has
// to follow the cursor.
func testServer() *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ServerOptions{PageSize: 2})
	obj := map[string]any{"type": "object"}
	text := func(s string) *sdk.CallToolResult {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}
	}
	add := func(name string, h func(args map[string]any) *sdk.CallToolResult) {
		srv.AddTool(&sdk.Tool{Name: name, Description: "test tool " + name, InputSchema: obj},
			func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				var args map[string]any
				_ = json.Unmarshal(req.Params.Arguments, &args)
				return h(args), nil
			})
	}
	add("echo", func(a map[string]any) *sdk.CallToolResult { return text(a["text"].(string)) })
	add("fail", func(map[string]any) *sdk.CallToolResult {
		r := text("boom")
		r.IsError = true
		return r
	})
	add("slow", func(a map[string]any) *sdk.CallToolResult {
		time.Sleep(time.Duration(a["ms"].(float64)) * time.Millisecond)
		return text("done")
	})
	add("mixed", func(map[string]any) *sdk.CallToolResult {
		return &sdk.CallToolResult{
			Content: []sdk.Content{
				&sdk.TextContent{Text: "here"},
				&sdk.ImageContent{MIMEType: "image/png", Data: []byte("png")},
			},
			StructuredContent: map[string]any{"n": 1},
		}
	})
	add("pid", func(map[string]any) *sdk.CallToolResult { return text(strconv.Itoa(os.Getpid())) })
	add("env", func(map[string]any) *sdk.CallToolResult { return text(strings.Join(os.Environ(), "\n")) })
	add("crash", func(map[string]any) *sdk.CallToolResult { os.Exit(3); return nil })
	add("spawn", func(map[string]any) *sdk.CallToolResult {
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			return text(err.Error())
		}
		return text(strconv.Itoa(cmd.Process.Pid))
	})
	add("dotted.name", func(map[string]any) *sdk.CallToolResult { return text("dot") })
	add(strings.Repeat("long_", 15), func(map[string]any) *sdk.CallToolResult { return text("long") })
	srv.AddTool(&sdk.Tool{Name: "noschema", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { return text("ok"), nil })
	return srv
}

type harness struct {
	m   *Manager
	reg *tools.Registry

	mu       sync.Mutex
	dials    int
	sessions []*sdk.ServerSession
}

// inMemory runs the test server in-process, one fresh server per dial.
func inMemory(t *testing.T, cfg *config.MCPServer) *harness {
	t.Helper()
	h := &harness{reg: tools.NewRegistry()}
	h.m = New(map[string]*config.MCPServer{"t": cfg}, h.reg, func(string) string { return "" }, nil)
	h.m.dial = func(*server) (sdk.Transport, error) {
		ct, st := sdk.NewInMemoryTransports()
		ss, err := testServer().Connect(context.Background(), st, nil)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.dials++
		h.sessions = append(h.sessions, ss)
		h.mu.Unlock()
		return ct, nil
	}
	t.Cleanup(h.m.Close)
	h.m.Start(context.Background())
	return h
}

func stdioServer(t *testing.T, cfg *config.MCPServer) *harness {
	t.Helper()
	cfg.Command = os.Args[0]
	cfg.Env = map[string]string{serverEnv: "MODE"}
	h := &harness{reg: tools.NewRegistry()}
	h.m = New(map[string]*config.MCPServer{"t": cfg}, h.reg, func(name string) string {
		if name == "MODE" {
			return "stdio"
		}
		return ""
	}, nil)
	t.Cleanup(h.m.Close)
	h.m.Start(context.Background())
	return h
}

func (h *harness) run(t *testing.T, mode tools.Mode, name string, input any) llm.Block {
	t.Helper()
	b, _ := json.Marshal(input)
	return h.reg.Execute(context.Background(), &tools.Env{Mode: mode},
		llm.Block{Type: llm.ToolUse, ToolUseID: "1", ToolName: name, Input: b})
}

func (h *harness) dialCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dials
}

func (h *harness) names() []string {
	var out []string
	for _, s := range h.reg.Specs([]string{"mcp:t"}) {
		out = append(out, s.Name)
	}
	return out
}

func TestListAndCall(t *testing.T) {
	h := inMemory(t, &config.MCPServer{Risk: "network"})
	names := h.names()
	if len(names) != 11 { // 11 tools over six pages
		t.Fatalf("registered %d tools: %v", len(names), names)
	}
	for _, want := range []string{"mcp__t__echo", "mcp__t__dotted_name", "mcp__t__noschema"} {
		if !slices.Contains(names, want) {
			t.Errorf("missing %s in %v", want, names)
		}
	}
	res := h.run(t, tools.ModeWorkspace, "mcp__t__echo", map[string]any{"text": "hi"})
	if res.IsError || res.Text != "hi" {
		t.Fatalf("echo: %+v", res)
	}
	res = h.run(t, tools.ModeWorkspace, "mcp__t__mixed", map[string]any{})
	if res.IsError || res.Text != "here\n[image image/png, 3 bytes, not shown]\n{\"n\":1}" {
		t.Fatalf("mixed: %q", res.Text)
	}
	for _, s := range h.reg.Specs([]string{"mcp:t"}) {
		if s.Name == "mcp__t__noschema" && (s.Description != "Tool noschema from the t MCP server." || string(s.Schema) != `{"type":"object"}`) {
			t.Fatalf("noschema spec: %+v", s)
		}
	}
	st := h.m.Status()
	if len(st) != 1 || st[0].State != "connected" || len(st[0].Tools) != 11 {
		t.Fatalf("status: %+v", st)
	}
}

func TestErrorResult(t *testing.T) {
	h := inMemory(t, &config.MCPServer{})
	res := h.run(t, tools.ModeFull, "mcp__t__fail", map[string]any{})
	if !res.IsError || res.Text != "error: boom" {
		t.Fatalf("fail: %+v", res)
	}
	// An error result is the tool's answer, not a broken connection.
	if res := h.run(t, tools.ModeFull, "mcp__t__echo", map[string]any{"text": "still here"}); res.Text != "still here" || h.dialCount() != 1 {
		t.Fatalf("after error: %+v, dials %d", res, h.dialCount())
	}
}

func TestToolNames(t *testing.T) {
	if got := toolName("gh", "list_issues"); got != "mcp__gh__list_issues" {
		t.Fatal(got)
	}
	if got := toolName("fs", "read.file/v2"); got != "mcp__fs__read_file_v2" {
		t.Fatal(got)
	}
	long := strings.Repeat("x", 80)
	a, b := toolName("srv", long+"a"), toolName("srv", long+"b")
	if len(a) != 64 || a == b || a != toolName("srv", long+"a") {
		t.Fatalf("long names: %q %q", a, b)
	}

	// Two remote names that sanitize alike still get distinct names.
	s := &server{name: "t", names: map[string]string{}}
	x, y := s.toolFor(&sdk.Tool{Name: "a.b"}), s.toolFor(&sdk.Tool{Name: "a_b"})
	if x.spec.Name != "mcp__t__a_b" || y.spec.Name == x.spec.Name || len(y.spec.Name) > 64 {
		t.Fatalf("collision: %q %q", x.spec.Name, y.spec.Name)
	}
	if again := s.toolFor(&sdk.Tool{Name: "a_b"}); again.spec.Name != y.spec.Name {
		t.Fatal("name changed between calls")
	}
	if d := s.toolFor(&sdk.Tool{Name: "d", Description: strings.Repeat("é", 800)}).spec.Description; len(d) > maxDescription+3 || !strings.HasSuffix(d, "...") {
		t.Fatalf("description not capped: %d bytes", len(d))
	}
}

func TestReconnectAfterServerDies(t *testing.T) {
	h := inMemory(t, &config.MCPServer{})
	h.mu.Lock()
	h.sessions[0].Close() // the server goes away
	h.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := h.run(t, tools.ModeFull, "mcp__t__echo", map[string]any{"text": "back"})
		if !res.IsError && res.Text == "back" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reconnected: %+v", res)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := h.dialCount(); n != 2 {
		t.Fatalf("dials = %d, want 2", n)
	}
}

func TestTimeout(t *testing.T) {
	h := inMemory(t, &config.MCPServer{Timeout: 200 * time.Millisecond})
	start := time.Now()
	res := h.run(t, tools.ModeFull, "mcp__t__slow", map[string]any{"ms": 3000})
	if !res.IsError || !strings.Contains(res.Text, "did not answer within 200ms") || time.Since(start) > 2*time.Second {
		t.Fatalf("slow: %+v after %s", res, time.Since(start))
	}
	if res := h.run(t, tools.ModeFull, "mcp__t__echo", map[string]any{"text": "ok"}); res.Text != "ok" {
		t.Fatalf("after timeout: %+v", res)
	}
}

func TestAllowAndApprove(t *testing.T) {
	h := inMemory(t, &config.MCPServer{Risk: "read", Allow: []string{"echo", "fail", "ghost"}, Approve: []string{"fail"}})
	if got := h.names(); !slices.Equal(got, []string{"mcp__t__echo", "mcp__t__fail"}) {
		t.Fatalf("allow: %v", got)
	}
	// read risk runs even in read-only mode; approve forces approval.
	if res := h.run(t, tools.ModeReadOnly, "mcp__t__echo", map[string]any{"text": "r"}); res.IsError {
		t.Fatalf("echo in read-only: %+v", res)
	}
	res := h.run(t, tools.ModeWorkspace, "mcp__t__fail", map[string]any{})
	if !res.IsError || !strings.Contains(res.Text, "needs approval") {
		t.Fatalf("approve: %+v", res)
	}
}

// silentServer is a program that runs and never speaks MCP.
func silentServer() *config.MCPServer {
	if platform.IsWindows {
		return &config.MCPServer{Command: "powershell", Args: []string{"-NoProfile", "-Command", "Start-Sleep 60"}}
	}
	return &config.MCPServer{Command: "sleep", Args: []string{"60"}}
}

func TestUnavailableServerDoesNotBlock(t *testing.T) {
	reg := tools.NewRegistry()
	m := New(map[string]*config.MCPServer{
		"missing": {Command: "/nonexistent/habakkuk-mcp-server"},
		"silent":  silentServer(), // never speaks MCP
		"nokey":   {Command: "true", Env: map[string]string{"TOKEN": "NOT_SET"}},
	}, reg, func(string) string { return "" }, nil)
	m.StartTimeout = 500 * time.Millisecond
	start := time.Now()
	m.Start(context.Background())
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Start blocked for %s", d)
	}
	byName := map[string]Status{}
	for _, st := range m.Status() {
		byName[st.Name] = st
	}
	if st := byName["missing"]; st.State != "error" || !strings.Contains(st.Error, "nonexistent") {
		t.Errorf("missing: %+v", st)
	}
	if st := byName["nokey"]; st.State != "error" || !strings.Contains(st.Error, "NOT_SET not set") {
		t.Errorf("nokey: %+v", st)
	}
	if st := byName["silent"]; st.State != "connecting" {
		t.Errorf("silent: %+v", st)
	}
	// A tool call on a server in backoff fails at once with the reason.
	s := m.servers["missing"]
	if _, err := s.call(context.Background(), "x", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("call: %v", err)
	}
	start = time.Now()
	m.Close()
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("Close took %s", d)
	}
}

func TestStdio(t *testing.T) {
	h := stdioServer(t, &config.MCPServer{})
	if n := len(h.names()); n != 11 {
		t.Fatalf("registered %d tools", n)
	}
	// The child sees PATH, HOME and its configured variables, nothing more
	// (on Windows also what programs need to start, such as SystemRoot).
	allowed := map[string]bool{"PATH": true, "HOME": true, serverEnv: true}
	if platform.IsWindows {
		for _, k := range platform.MinimalEnvKeys() {
			allowed[strings.ToUpper(k)] = true
		}
	}
	res := h.run(t, tools.ModeFull, "mcp__t__env", map[string]any{})
	for _, kv := range strings.Split(res.Text, "\n") {
		k, _, _ := strings.Cut(strings.TrimSpace(kv), "=")
		if platform.IsWindows && (k == "" || allowed[strings.ToUpper(k)]) {
			continue // "=C:" entries are per-drive working directories
		}
		if !allowed[k] {
			t.Errorf("child environment leaked %s", k)
		}
	}
	if !strings.Contains(res.Text, serverEnv+"=stdio") {
		t.Fatalf("configured variable missing: %q", res.Text)
	}

	pid := h.run(t, tools.ModeFull, "mcp__t__pid", map[string]any{}).Text
	res = h.run(t, tools.ModeFull, "mcp__t__crash", map[string]any{})
	if !res.IsError {
		t.Fatalf("crash did not fail: %+v", res)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		res = h.run(t, tools.ModeFull, "mcp__t__pid", map[string]any{})
		if !res.IsError {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never restarted: %+v", res)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if res.Text == pid {
		t.Fatal("same process after crash")
	}

	// Close kills the server's whole process group, helpers included.
	t.Run("close kills helpers", func(t *testing.T) {
		testutil.UnixOnly(t, "spawns sleep and relies on process groups; taskkill /T cannot reach a helper whose server already exited")
		child, err := strconv.Atoi(h.run(t, tools.ModeFull, "mcp__t__spawn", map[string]any{}).Text)
		if err != nil {
			t.Fatal(err)
		}
		h.m.Close()
		deadline := time.Now().Add(5 * time.Second)
		for platform.Alive(child) {
			if time.Now().After(deadline) {
				_ = platform.Kill(child)
				t.Fatal("helper process survived Close")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

func TestLateServerRegistersWhenUp(t *testing.T) {
	reg := tools.NewRegistry()
	var mu sync.Mutex
	up := false
	m := New(map[string]*config.MCPServer{"t": {}}, reg, func(string) string { return "" }, nil)
	m.StartTimeout = 300 * time.Millisecond
	m.dial = func(*server) (sdk.Transport, error) {
		mu.Lock()
		defer mu.Unlock()
		if !up {
			return nil, errors.New("not yet")
		}
		ct, st := sdk.NewInMemoryTransports()
		if _, err := testServer().Connect(context.Background(), st, nil); err != nil {
			return nil, err
		}
		return ct, nil
	}
	defer m.Close()
	m.Start(context.Background())
	if n := len(reg.Specs([]string{"mcp:t"})); n != 0 {
		t.Fatalf("%d tools before the server is up", n)
	}
	mu.Lock()
	up = true
	mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for len(reg.Specs([]string{"mcp:t"})) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("tools never registered")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A slow server must not hold up startup; a turn that uses it waits for it
// briefly, and learns it is still connecting when the wait runs out.
func TestBackgroundStartAndPending(t *testing.T) {
	reg := tools.NewRegistry()
	release := make(chan struct{})
	m := New(map[string]*config.MCPServer{"slow": {}, "fast": {}}, reg, func(string) string { return "" }, nil)
	m.dial = func(s *server) (sdk.Transport, error) {
		if s.name == "slow" {
			<-release
		}
		ct, st := sdk.NewInMemoryTransports()
		if _, err := testServer().Connect(context.Background(), st, nil); err != nil {
			return nil, err
		}
		return ct, nil
	}
	defer m.Close()
	defer close(release)
	ctx := context.Background()
	if got := m.Pending(ctx, []string{"mcp:slow"}, time.Second); got != nil {
		t.Fatalf("pending before start: %v", got)
	}
	start := time.Now()
	m.StartBackground()
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("StartBackground blocked for %s", d)
	}
	if got := m.Pending(ctx, []string{"core", "mcp:fast"}, 5*time.Second); len(got) != 0 {
		t.Fatalf("fast server still pending: %v", got)
	}
	if n := len(reg.Specs([]string{"mcp:fast"})); n == 0 {
		t.Fatal("the fast server's tools are not registered after waiting for it")
	}
	if got := m.Pending(ctx, []string{"mcp:fast", "mcp:slow"}, 100*time.Millisecond); !slices.Equal(got, []string{"slow"}) {
		t.Fatalf("pending = %v, want [slow]", got)
	}
}

func TestFormatResult(t *testing.T) {
	res := &sdk.CallToolResult{
		Content: []sdk.Content{
			&sdk.TextContent{Text: `{"a": 1}`},
		},
		StructuredContent: map[string]any{"a": 1},
	}
	if got := formatResult(res); got != `{"a": 1}` {
		t.Fatalf("duplicate structured content: %q", got)
	}
	res = &sdk.CallToolResult{Content: []sdk.Content{
		&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///a", Text: "body"}},
		&sdk.ResourceLink{Name: "b", URI: "file:///b"},
	}}
	if got := formatResult(res); got != "[resource file:///a]\nbody\n[resource link b: file:///b]" {
		t.Fatalf("resources: %q", got)
	}
}

func TestHeaderValue(t *testing.T) {
	env := map[string]string{"KEY": "k1", "WHOLE": "Bearer w"}
	m := New(nil, tools.NewRegistry(), func(n string) string { return env[n] }, nil)
	s := &server{m: m}
	for spec, want := range map[string]string{"WHOLE": "Bearer w", "Bearer ${KEY}": "Bearer k1", "${KEY}-${KEY}": "k1-k1"} {
		if got, missing := s.headerValue(spec); got != want || len(missing) != 0 {
			t.Errorf("%q = %q, missing %v; want %q", spec, got, missing, want)
		}
	}
	if _, missing := s.headerValue("Bearer ${NOPE}"); len(missing) != 1 || missing[0] != "NOPE" {
		t.Errorf("missing = %v", missing)
	}
}

func TestHeaderVars(t *testing.T) {
	if got := HeaderVars("Bearer ${A} ${B}"); !slices.Equal(got, []string{"A", "B"}) {
		t.Errorf("template vars = %v", got)
	}
	if got := HeaderVars("WHOLE"); !slices.Equal(got, []string{"WHOLE"}) {
		t.Errorf("plain = %v", got)
	}
}
