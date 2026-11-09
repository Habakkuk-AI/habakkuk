package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/config"
	plat "github.com/habakkuk-ai/habakkuk/internal/core/platform"
	"github.com/habakkuk-ai/habakkuk/internal/protocols/nodes"
)

func TestNodeCLI(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	cfg := config.Template + "language: en\nhttp:\n  listen: 127.0.0.1:9000\nnodes: {}\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := nodesCmd(ctx, home, nil, &out); err != nil || !strings.Contains(out.String(), "No nodes are paired yet") {
		t.Fatalf("empty list: %v %q", err, out.String())
	}
	out.Reset()
	if err := nodePair(ctx, home, nil, &out); err != nil {
		t.Fatal(err)
	}
	code := regexp.MustCompile(`-code ([A-Z0-9]{5}-[A-Z0-9]{5})`).FindStringSubmatch(out.String())
	if code == nil || !strings.Contains(out.String(), "-server http://127.0.0.1:9000") || !strings.Contains(out.String(), "/app/pair.html#"+code[1]) {
		t.Fatalf("pair output:\n%s", out.String())
	}

	// Redeem the code as a device would (straight against the store).
	st, err := nodes.OpenStore(ctx, filepath.Join(home, "nodes.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Redeem(ctx, code[1], "laptop", "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	out.Reset()
	if err := nodesCmd(ctx, home, []string{"list"}, &out); err != nil || !strings.Contains(out.String(), "laptop") || !strings.Contains(out.String(), "never connected") {
		t.Fatalf("list: %v %q", err, out.String())
	}
	out.Reset()
	if err := nodesCmd(ctx, home, []string{"revoke", "laptop"}, &out); err != nil || !strings.Contains(out.String(), "Revoked laptop") {
		t.Fatalf("revoke: %v %q", err, out.String())
	}
	if err := nodesCmd(ctx, home, []string{"revoke", "laptop"}, &out); err == nil {
		t.Fatal("revoking twice succeeded")
	}
}

func TestNodeCLIWithoutNodes(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(config.Template), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := nodesCmd(context.Background(), home, nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "nodes are off") {
		t.Fatalf("got %v", err)
	}
}

func TestNodeStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "node.json")
	if err := saveNodeState(path, &nodeState{Server: "https://x", Name: "pi", Secret: "s3cret"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || (plat.HasUnixPermissions && fi.Mode().Perm() != 0o600) {
		t.Fatalf("state file mode: %v %v", err, fi.Mode())
	}
	st, err := loadNodeState(path)
	if err != nil || st.Name != "pi" || st.Secret != "s3cret" {
		t.Fatalf("load: %v %+v", err, st)
	}
}
