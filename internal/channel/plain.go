package channel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/ops/i18n"
	"github.com/habakkuk-ai/habakkuk/internal/ops/logs"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// Plain words for a chat. A person who is not technical should never have
// to read a tool name, a command, a path, a job id or JSON to follow what
// the agent is doing or to decide whether to allow it. Everything here is
// derived from the tool call alone, with no model call, so the same call
// always reads the same and nothing a web page planted can change the
// wording.

// The kinds of work a tool call is, as a person would name them.
const (
	workOther = iota
	workFiles
	workWeb
	workNotes
	workHandoff
	workSchedule
	workMCP
	workMedia
)

// workKind sorts a tool into the kind of work it is.
func workKind(tool string) int {
	has := func(prefixes ...string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(tool, p) {
				return true
			}
		}
		return false
	}
	switch {
	case has("mcp__"):
		return workMCP
	case has("process_"):
		return workFiles
	case has("browser_"):
		return workWeb
	case has("memory_", "user_profile_", "skill_"):
		return workNotes
	case has("schedule_"):
		return workSchedule
	}
	switch tool {
	case "exec", "read_file", "write_file", "edit_file", "apply_patch", "list_dir", "search_files",
		"read_document", "view_image", "send_file":
		return workFiles
	case "web_search", "fetch_url":
		return workWeb
	case "session_search", "todo":
		return workNotes
	case "delegate", "board_create", "a2a_send":
		return workHandoff
	case "remind":
		return workSchedule
	case "image_generate", "video_generate", "speak":
		return workMedia
	}
	return workOther
}

// plainPhrase is the one line a chat shows while a tool runs, in lang.
func (g *Gateway) plainPhrase(lang, tool string, input json.RawMessage) string {
	switch workKind(tool) {
	case workFiles:
		return i18n.T(lang, "progress.files")
	case workWeb:
		return i18n.T(lang, "progress.web")
	case workNotes:
		return i18n.T(lang, "progress.notes")
	case workHandoff:
		if name := g.handoffName(input); name != "" {
			return i18n.T(lang, "progress.handoff", name)
		}
		return i18n.T(lang, "progress.handoff_helper")
	case workSchedule:
		return i18n.T(lang, "progress.schedule")
	case workMCP:
		if server, _ := mcpParts(tool); server != "" {
			return i18n.T(lang, "progress.mcp", humanName(server))
		}
	case workMedia:
		return i18n.T(lang, "progress.media")
	}
	return i18n.T(lang, "progress.working")
}

// handoffName is the name of the agent a delegate or board call hands work
// to, as that agent introduces itself; "" when the call names none.
func (g *Gateway) handoffName(input json.RawMessage) string {
	var in struct {
		Agent    string `json:"agent"`
		Assignee string `json:"assignee"`
	}
	_ = json.Unmarshal(input, &in)
	name := strings.TrimSpace(in.Agent)
	if name == "" {
		name = strings.TrimSpace(in.Assignee)
	}
	if name == "" || strings.Contains(name, "://") { // an A2A address is not a name
		return ""
	}
	if g.Runner != nil {
		if spec, ok := g.Runner.Agents[name]; ok {
			return clean(agent.DisplayName(name, spec.Dir), 40)
		}
	}
	return humanName(name)
}

// mcpParts splits mcp__<server>__<tool>.
func mcpParts(tool string) (server, remote string) {
	rest, ok := strings.CutPrefix(tool, "mcp__")
	if !ok {
		return "", ""
	}
	server, remote, _ = strings.Cut(rest, "__")
	return server, remote
}

// wellKnown are names whose usual spelling a capital letter does not give.
var wellKnown = map[string]string{
	"gh": "GitHub", "github": "GitHub", "gitlab": "GitLab", "gcal": "Google Calendar", "gmail": "Gmail",
	"gdrive": "Google Drive", "hubspot": "HubSpot", "quickbooks": "QuickBooks",
	"whatsapp": "WhatsApp", "youtube": "YouTube", "linkedin": "LinkedIn",
	"cal": "Calendar", "db": "Database",
}

// humanName turns an identifier (google_calendar, bramblecrest-bakery) into
// words with capitals (Google Calendar, Bramblecrest Bakery).
func humanName(id string) string {
	if n, ok := wellKnown[strings.ToLower(id)]; ok {
		return n
	}
	words := strings.FieldsFunc(id, func(r rune) bool { return r == '_' || r == '-' || r == '.' || unicode.IsSpace(r) })
	for i, w := range words {
		if n, ok := wellKnown[strings.ToLower(w)]; ok {
			words[i] = n
			continue
		}
		r, size := utf8.DecodeRuneInString(w)
		words[i] = string(unicode.ToUpper(r)) + w[size:]
	}
	return clean(strings.Join(words, " "), 40)
}

// humanAction turns a tool's own name into lower-case words
// (create_submission -> "create submission").
func humanAction(id string) string {
	words := strings.FieldsFunc(id, func(r rune) bool { return r == '_' || r == '-' || r == '.' || unicode.IsSpace(r) })
	return clean(strings.ToLower(strings.Join(words, " ")), 60)
}

// clean makes a value safe to drop into a sentence: one line, no markup
// characters, at most n runes.
func clean(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '`' || r == '*' || r == '_' || r == '<' || r == '>' || r == '[' || r == ']':
			return ' '
		case unicode.IsControl(r):
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n-1]) + "…"
	}
	return s
}

// A shell command the description names: only a command simple enough to
// be sure of. Anything else is "a command".
const special = "|&;<>`$()\n\\"

var (
	reCd       = regexp.MustCompile(`^cd\s+([^\s` + regexp.QuoteMeta(special) + `]+)$`)
	reHeredoc  = regexp.MustCompile(`<<-?\s*(['"]?)([A-Za-z_][A-Za-z0-9_]*)(['"]?)`)
	reAppend   = regexp.MustCompile(`^(?:cat|echo|printf)\b([^` + regexp.QuoteMeta(special) + `]*)>>\s*([^\s'"*?` + regexp.QuoteMeta(special) + `]+)$`)
	reRm       = regexp.MustCompile(`^rm((?:\s+-[A-Za-z]+)*)((?:\s+[^\s'"` + regexp.QuoteMeta(special) + `]+)+)$`)
	reGitPush  = regexp.MustCompile(`^git\s+push\b[^` + regexp.QuoteMeta(special) + `]*$`)
	reFetch    = regexp.MustCompile(`^(?:curl|wget)\b[^` + regexp.QuoteMeta(special) + `]*$`)
	reURL      = regexp.MustCompile(`https?://[^\s'"]+`)
	reSendFlag = regexp.MustCompile(`(?:^|\s)(?:-d|--data\S*|-F|--form\S*|-T|--upload-file|--post-data\S*|--post-file\S*|--json|-X\s*(?:POST|PUT|PATCH|DELETE)|--request\s*(?:POST|PUT|PATCH|DELETE))(?:\s|=|$)`)
)

// shellIntent is what a shell command does, when it is plain enough to say.
type shellIntent struct {
	verb   string // append, delete, delete_tree, delete_many, git_push, fetch, send, or "" for "a command"
	target string // the file or folder it changes, as written in the command
	dir    string // the folder a leading cd moved to, as written
	host   string // the site a fetch contacts
}

// readCommand works out what a shell command does. It names the action
// only when the command is one simple step (after any leading cd): several
// steps, pipes, substitutions or redirections beyond the one it knows all
// read as "a command", so the description never says less than the command
// does.
func readCommand(cmd string) shellIntent {
	var in shellIntent
	cmd = strings.TrimSpace(cmd)
	// A heredoc's body is text, not commands; anything after it is more
	// commands.
	if m := reHeredoc.FindStringSubmatchIndex(cmd); m != nil {
		word := cmd[m[4]:m[5]]
		head, body, _ := strings.Cut(cmd[m[1]:], "\n")
		if strings.TrimSpace(head) != "" {
			return in
		}
		lines := strings.Split(body, "\n")
		end := -1
		for i, l := range lines {
			if strings.TrimSpace(l) == word {
				end = i
				break
			}
		}
		if end >= 0 && strings.TrimSpace(strings.Join(lines[end+1:], "")) != "" {
			return in
		}
		cmd = strings.TrimSpace(cmd[:m[0]])
	}
	if strings.ContainsAny(cmd, "\n") {
		return in
	}
	steps := strings.Split(cmd, "&&")
	for len(steps) > 1 {
		m := reCd.FindStringSubmatch(strings.TrimSpace(steps[0]))
		if m == nil {
			return in
		}
		in.dir, steps = m[1], steps[1:]
	}
	step := strings.TrimSpace(steps[0])
	switch {
	case reAppend.MatchString(step):
		in.verb, in.target = "append", reAppend.FindStringSubmatch(step)[2]
	case reRm.MatchString(step):
		m := reRm.FindStringSubmatch(step)
		targets := strings.Fields(m[2])
		switch {
		case len(targets) != 1 || strings.ContainsAny(targets[0], "*?"):
			in.verb = "delete_many"
		case strings.ContainsAny(m[1], "rR"):
			in.verb, in.target = "delete_tree", targets[0]
		default:
			in.verb, in.target = "delete", targets[0]
		}
	case reGitPush.MatchString(step):
		in.verb = "git_push"
	case reFetch.MatchString(step):
		in.verb = "fetch"
		if reSendFlag.MatchString(step) {
			in.verb = "send"
		}
		if u, err := url.Parse(reURL.FindString(step)); err == nil {
			in.host = u.Hostname()
		}
	}
	return in
}

// place names the workspace a path is in, in lang ("your Bramblecrest Bakery
// workspace"), or "" when it is in none: the workspace of the agent that
// asks, or of another configured agent.
func (g *Gateway) place(lang, path, workspace string) string {
	inside := func(ws string) bool {
		if ws == "" || path == "" {
			return false
		}
		rel, err := filepath.Rel(ws, path)
		return err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	name := ""
	switch {
	case inside(workspace):
		name = filepath.Base(workspace)
	case g.Runner != nil:
		for _, spec := range g.Runner.Agents {
			if spec != nil && inside(spec.Workspace) {
				name = filepath.Base(spec.Workspace)
				break
			}
		}
	}
	if name == "" {
		return ""
	}
	name = strings.Trim(strings.TrimSuffix(strings.ToLower(name), "workspace"), "-_. ")
	if name == "" || name == "ws" {
		return i18n.T(lang, "approval.place.own")
	}
	return i18n.T(lang, "approval.place.named", humanName(name))
}

// noteLike reports whether a file reads as notes rather than as settings
// or a program.
func noteLike(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".txt", ".text", ".org", ".rst":
		return true
	}
	return false
}

// shortPath shows where a file outside the workspace is without the whole
// path: its last two elements, and whether it is under the user's home (a
// path tildePath wrote with ~).
func shortPath(path string) string {
	path = filepath.ToSlash(path)
	prefix := ""
	switch {
	case path == "~" || path == "/":
		return path
	case strings.HasPrefix(path, "~/"):
		prefix, path = "~/", path[2:]
	case strings.HasPrefix(path, "/"):
		prefix = "/"
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > 2 {
		parts = append([]string{"…"}, parts[len(parts)-2:]...)
		if prefix == "/" {
			prefix = ""
		}
	}
	return clean(prefix+strings.Join(parts, "/"), 80)
}

// fileName is a path's last element, safe for a sentence.
func fileName(path string) string {
	return clean(filepath.Base(filepath.ToSlash(strings.TrimRight(path, "/\\"))), 60)
}

// resolveIn makes a path from a command absolute: under ~, under dir (a
// leading cd), or under the workspace.
func resolveIn(env *tools.Env, dir, p string) string {
	if p == "" {
		return ""
	}
	if dir != "" && !filepath.IsAbs(p) && !strings.HasPrefix(p, "~") {
		return filepath.Join(env.Resolve(dir), p)
	}
	return env.Resolve(p)
}

// describeCall says what a tool call wants to do, in one everyday sentence
// in lang.
func (g *Gateway) describeCall(lang string, req tools.ApprovalRequest) string {
	t := func(key string, args ...any) string { return i18n.T(lang, key, args...) }
	env := &tools.Env{Workspace: req.Workspace}
	var in struct {
		Command string `json:"command"`
		Path    string `json:"path"`
		Patch   string `json:"patch"`
		URL     string `json:"url"`
		To      any    `json:"to"`
	}
	_ = json.Unmarshal(req.Input, &in)
	switch tool := req.Tool; {
	case tool == "exec":
		c := readCommand(in.Command)
		target := resolveIn(env, c.dir, c.target)
		where := g.place(lang, target, req.Workspace)
		outside := shortPath(tildePath(target))
		switch c.verb {
		case "append":
			switch {
			case where == "":
				return t("approval.do.append_out", outside)
			case noteLike(target):
				return t("approval.do.append_in", fileName(target), where)
			}
			return t("approval.do.append_text_in", fileName(target), where)
		case "delete":
			if where == "" {
				return t("approval.do.delete_out", outside)
			}
			return t("approval.do.delete_in", fileName(target), where)
		case "delete_tree":
			return t("approval.do.delete_tree", outside)
		case "delete_many":
			return t("approval.do.delete_many")
		case "git_push":
			return t("approval.do.git_push")
		case "fetch", "send":
			key := "approval.do.curl"
			if c.verb == "send" {
				key = "approval.do.curl_send"
			}
			if c.host == "" {
				return t(key + "_any")
			}
			return t(key, clean(c.host, 60))
		}
		return t("approval.do.exec")
	case strings.HasPrefix(tool, "process_"):
		return t("approval.do.process")
	case tool == "write_file" || tool == "edit_file":
		abs := env.Resolve(in.Path)
		if where := g.place(lang, abs, req.Workspace); where != "" {
			return t("approval.do.write_in", fileName(abs), where)
		}
		return t("approval.do.write_out", shortPath(tildePath(abs)))
	case tool == "apply_patch":
		paths := patchPaths(in.Patch)
		if len(paths) == 1 {
			abs := env.Resolve(paths[0])
			if where := g.place(lang, abs, req.Workspace); where != "" {
				return t("approval.do.write_in", fileName(abs), where)
			}
			return t("approval.do.write_out", shortPath(tildePath(abs)))
		}
		return t("approval.do.write_many")
	case tool == "send_file":
		return t("approval.do.send_file")
	case tool == "send_email":
		return t("approval.do.email_own")
	case strings.HasPrefix(tool, "mcp__"):
		server, remote := mcpParts(tool)
		name, action := humanName(server), humanAction(remote)
		switch {
		case strings.Contains(action, "send") && (strings.Contains(action, "mail")):
			return t("approval.do.email", name)
		case strings.Contains(action, "send") && (strings.Contains(action, "message") || strings.Contains(action, "sms") || strings.Contains(action, "text")):
			return t("approval.do.message", name)
		case action == "":
			return t("approval.do.other", name)
		}
		return t("approval.do.mcp", name, action)
	case strings.HasPrefix(tool, "browser_"):
		if u, err := url.Parse(strings.TrimSpace(in.URL)); err == nil && u.Hostname() != "" {
			return t("approval.do.browser", clean(u.Hostname(), 60))
		}
		return t("approval.do.browser_any")
	case strings.HasPrefix(tool, "computer_"):
		return t("approval.do.computer")
	case strings.HasPrefix(tool, "ha_"):
		return t("approval.do.home")
	case tool == "a2a_send":
		return t("approval.do.agent")
	case tool == "node_invoke":
		return t("approval.do.node")
	}
	return t("approval.do.other", humanAction(req.Tool))
}

// tildePath writes a path under the user's home with ~.
func tildePath(abs string) string {
	home := filepath.ToSlash((&tools.Env{}).Resolve("~/"))
	if home != "" && home != "/" && home != "." {
		if filepath.ToSlash(abs) == home {
			return "~"
		}
		if rest, ok := strings.CutPrefix(filepath.ToSlash(abs), strings.TrimSuffix(home, "/")+"/"); ok {
			return "~/" + rest
		}
	}
	return abs
}

var rePatchPath = regexp.MustCompile(`(?m)^(?:\*\*\* (?:Add|Update|Delete) File: |\*\*\* Move to: |\+\+\+ (?:b/)?)(\S.*?)\s*$`)

// patchPaths lists the files a patch names, each once.
func patchPaths(patch string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range rePatchPath.FindAllStringSubmatch(patch, -1) {
		if p := m[1]; p != "/dev/null" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// why says, in lang, why the agent is asking; "" when the reason is only
// the permission mode.
func (g *Gateway) why(lang string, req tools.ApprovalRequest) string {
	switch req.Cause {
	case tools.CauseTainted:
		return i18n.T(lang, "approval.why.tainted")
	case tools.CauseChannel:
		return i18n.T(lang, "approval.why.channel")
	case tools.CauseConfirm:
		return i18n.T(lang, "approval.why.confirm")
	}
	if r := strings.TrimSpace(req.Review); r != "" { // the reviewer handed it over
		return i18n.T(lang, "approval.why.review", clean(g.redact(r), 300))
	}
	return ""
}

// Lengths of the exact call shown in a chat: on request, and inside a
// prompt on channels with approvals_detail: full.
const (
	maxDetail       = 1500
	maxDetailInline = 700
)

// exactCall is the call an approval is about, word for word: the command
// for the shell, else the tool and its input. Secrets are redacted and it
// is cut to n runes.
func (g *Gateway) exactCall(req tools.ApprovalRequest, n int) string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(req.Input, &in)
	text := in.Command
	if req.Tool != "exec" || strings.TrimSpace(text) == "" {
		var pretty bytes.Buffer
		switch {
		case len(req.Input) > 0 && json.Indent(&pretty, req.Input, "", "  ") == nil:
			text = req.Tool + "\n" + pretty.String()
		case len(req.Input) > 0:
			text = req.Tool + "\n" + string(req.Input)
		default:
			text = req.Tool + "\n" + req.Summary
		}
	}
	text = strings.ReplaceAll(g.redact(strings.TrimSpace(text)), "```", "'''")
	if utf8.RuneCountInString(text) > n {
		text = string([]rune(text)[:n]) + "\n…"
	}
	return "```\n" + text + "\n```"
}

// redact removes secrets from text shown in a chat.
func (g *Gateway) redact(s string) string {
	if g.Redact != nil {
		return g.Redact(s)
	}
	return logs.RedactPatterns(s)
}

// callHash identifies one call of one conversation across restarts: the
// agent, the session, the tool and its input. A prompt's buttons carry it,
// so a button from before a restart can answer the same call when it is
// asked again, and nothing else.
func callHash(agentName, session string, req tools.ApprovalRequest) string {
	input := []byte(req.Summary)
	if len(req.Input) > 0 {
		input = req.Input
		var v any
		if json.Unmarshal(req.Input, &v) == nil { // the same call, however it was spaced or ordered
			if canon, err := json.Marshal(v); err == nil {
				input = canon
			}
		}
	}
	h := sha256.New()
	for _, part := range [][]byte{[]byte(agentName), []byte(session), []byte(req.Tool), input} {
		h.Write(part)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}
