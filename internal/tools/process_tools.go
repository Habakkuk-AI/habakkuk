package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// registerWorkTools adds the background process, patch and search tools to
// the core toolset.
func registerWorkTools(r *Registry) {
	r.Register("core", processList{})
	r.Register("core", processOutput{})
	r.Register("core", processWait{})
	r.Register("core", processKill{})
	r.Register("core", processWrite{})
	r.Register("core", applyPatch{})
	r.Register("core", searchFiles{})
}

func procsOf(env *Env) (*Procs, error) {
	if env.Procs == nil {
		return nil, errors.New("background processes are not available here")
	}
	return env.Procs, nil
}

func procOf(env *Env, id string) (*Proc, error) {
	ps, err := procsOf(env)
	if err != nil {
		return nil, err
	}
	return ps.Get(env.Agent, id)
}

// tail is the end of a process's output, for status replies.
func tail(p *Proc, n int) string {
	c, err := p.Output(-int64(n), n)
	if err != nil || len(c.Data) == 0 {
		return ""
	}
	s := string(c.Data)
	if c.From > 0 {
		s = "...\n" + s
	}
	return "\n\nLast output:\n" + s
}

// process_list

type processList struct{}

func (processList) Risk() Risk { return RiskRead }
func (processList) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "process_list",
		Description: "List this agent's background processes (started with exec background: true) and their status.",
		Schema:      schema(`{"type":"object","properties":{}}`),
	}
}

func (processList) Run(ctx context.Context, env *Env, input json.RawMessage) (string, error) {
	ps, err := procsOf(env)
	if err != nil {
		return "", err
	}
	list := ps.List(env.Agent)
	if len(list) == 0 {
		return "no background processes", nil
	}
	var b strings.Builder
	for _, p := range list {
		c, _ := p.Output(0, 0)
		fmt.Fprintf(&b, "%s  %s  %d bytes of output  %s\n", p.ID, p.Status(), c.Total, oneLine(p.Command, 100))
	}
	return strings.TrimSpace(b.String()), nil
}

// process_output

type processOutput struct{}

func (processOutput) Risk() Risk { return RiskRead }
func (processOutput) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "process_output",
		Description: "Read a background process's output (stdout and stderr together) from byte offset since (default 0). " +
			"The reply ends with the offset to pass next time. A negative since reads the last -since bytes. Very old output of chatty processes is discarded.",
		Schema: schema(`{"type":"object","properties":{
			"id":{"type":"string"},
			"since":{"type":"integer","description":"byte offset to read from; negative for the last N bytes"},
			"max_bytes":{"type":"integer","description":"default 16000, at most 64000"}},"required":["id"]}`),
	}
}

func (processOutput) Run(ctx context.Context, env *Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		ID       string `json:"id"`
		Since    int64  `json:"since"`
		MaxBytes int    `json:"max_bytes"`
	}](input)
	if err != nil {
		return "", err
	}
	p, err := procOf(env, in.ID)
	if err != nil {
		return "", err
	}
	limit := 16000
	if in.MaxBytes > 0 {
		limit = min(in.MaxBytes, 64000)
	}
	c, err := p.Output(in.Since, limit)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s %s; bytes %d-%d of %d]\n", p.ID, p.Status(), c.From, c.Next, c.Total)
	if c.Dropped > 0 {
		fmt.Fprintf(&b, "[%d earlier bytes were discarded by the output cap]\n", c.Dropped)
	}
	if len(c.Data) == 0 {
		b.WriteString("(no new output)\n")
	} else {
		b.Write(c.Data)
		if c.Data[len(c.Data)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	if c.Next < c.Total {
		fmt.Fprintf(&b, "[more output; continue with since=%d]", c.Next)
	} else {
		fmt.Fprintf(&b, "[next since=%d]", c.Next)
	}
	return b.String(), nil
}

// process_wait

type processWait struct{}

func (processWait) Risk() Risk { return RiskRead }
func (processWait) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "process_wait",
		Description: "Wait for a background process to exit, up to timeout_seconds (default 30, at most 600). Returns its status and the end of its output.",
		Schema: schema(`{"type":"object","properties":{
			"id":{"type":"string"},
			"timeout_seconds":{"type":"integer"}},"required":["id"]}`),
	}
}

func (processWait) Run(ctx context.Context, env *Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		ID             string `json:"id"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}](input)
	if err != nil {
		return "", err
	}
	p, err := procOf(env, in.ID)
	if err != nil {
		return "", err
	}
	d := 30 * time.Second
	if in.TimeoutSeconds > 0 {
		d = min(time.Duration(in.TimeoutSeconds)*time.Second, 10*time.Minute)
	}
	if !p.Wait(ctx, d) && ctx.Err() == nil {
		return fmt.Sprintf("%s is still %s", p.ID, p.Status()) + tail(p, 2000), nil
	}
	return fmt.Sprintf("%s %s", p.ID, p.Status()) + tail(p, 4000), ctx.Err()
}

// process_kill

type processKill struct{}

func (processKill) Risk() Risk { return RiskWrite }

// OwnState: it only stops a process this agent started.
func (processKill) OwnState() {}
func (processKill) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "process_kill",
		Description: "Stop a background process and everything it started.",
		Schema:      schema(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	}
}

func (processKill) Run(ctx context.Context, env *Env, input json.RawMessage) (string, error) {
	in, err := decode[struct{ ID string }](input)
	if err != nil {
		return "", err
	}
	p, err := procOf(env, in.ID)
	if err != nil {
		return "", err
	}
	if !p.Running() {
		return fmt.Sprintf("%s already %s", p.ID, p.Status()), nil
	}
	if !p.Kill(5 * time.Second) {
		return "", fmt.Errorf("%s did not exit within 5s of being killed", p.ID)
	}
	return fmt.Sprintf("%s %s", p.ID, p.Status()), nil
}

// process_write

type processWrite struct{}

func (processWrite) Risk() Risk { return RiskExec }
func (processWrite) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "process_write",
		Description: "Write text to a background process's standard input. Include a trailing newline to send a line. eof: true closes its input afterwards.",
		Schema: schema(`{"type":"object","properties":{
			"id":{"type":"string"},
			"stdin":{"type":"string"},
			"eof":{"type":"boolean"}},"required":["id","stdin"]}`),
	}
}

func (processWrite) Run(ctx context.Context, env *Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		ID    string `json:"id"`
		Stdin string `json:"stdin"`
		EOF   bool   `json:"eof"`
	}](input)
	if err != nil {
		return "", err
	}
	p, err := procOf(env, in.ID)
	if err != nil {
		return "", err
	}
	if err := p.Write(in.Stdin, in.EOF); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("wrote %d bytes to %s", len(in.Stdin), p.ID)
	if in.EOF {
		msg += " and closed its input"
	}
	return msg, nil
}
