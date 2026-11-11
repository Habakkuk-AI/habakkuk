package acp

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/habakkuk-ai/habakkuk/internal/agent"
	"github.com/habakkuk-ai/habakkuk/internal/tools"
)

// Stubs for the interface-typed fields of agent.Runner: each embeds the
// interface, so it satisfies it without implementing anything. They are
// only compared, never called.
type (
	stubSender        struct{ agent.Sender }
	stubOverlay       struct{ agent.Overlay }
	stubHooks         struct{ agent.Hooks }
	stubHome          struct{ agent.HomeReporter }
	stubSessionModels struct{ agent.SessionModels }
	stubBudget        struct{ agent.Budget }
	stubNotes         struct{ agent.ChatNotes }
	stubTrust         struct{ agent.ProjectTrust }
)

var interfaceStubs = map[reflect.Type]any{
	reflect.TypeFor[agent.Sender]():        stubSender{},
	reflect.TypeFor[agent.Overlay]():       stubOverlay{},
	reflect.TypeFor[agent.Hooks]():         stubHooks{},
	reflect.TypeFor[agent.HomeReporter]():  stubHome{},
	reflect.TypeFor[agent.SessionModels](): stubSessionModels{},
	reflect.TypeFor[agent.Budget]():        stubBudget{},
	reflect.TypeFor[agent.ChatNotes]():     stubNotes{},
	reflect.TypeFor[agent.ProjectTrust]():  stubTrust{},
}

// editorOwn are the agent.Runner fields the editor's runner sets itself
// instead of copying: its agents (with the editor toolset added) and its
// environment (the editor's folder and file system). Every other exported
// field must be carried over from the base runner, because it decides what
// a turn may do or what it sees: the tool policy, the trusted project
// files, hooks, budgets, chat notes and the rest.
var editorOwn = map[string]bool{
	"Agents": true, // each spec copied, with the editor toolset added
	"Env":    true, // wraps the base Env with the editor's folder
}

// nonZero makes a value of type t that is not its zero value.
func nonZero(t *testing.T, name string, ft reflect.Type) reflect.Value {
	t.Helper()
	switch ft.Kind() {
	case reflect.Pointer:
		return reflect.New(ft.Elem())
	case reflect.Func:
		return reflect.MakeFunc(ft, func([]reflect.Value) []reflect.Value {
			out := make([]reflect.Value, ft.NumOut())
			for i := range out {
				out[i] = reflect.Zero(ft.Out(i))
			}
			return out
		})
	case reflect.Map:
		m := reflect.MakeMap(ft)
		if ft == reflect.TypeFor[map[string]*agent.Spec]() {
			m.SetMapIndex(reflect.ValueOf("habakkuk"), reflect.ValueOf(&agent.Spec{Name: "habakkuk"}))
		}
		return m
	case reflect.String:
		return reflect.ValueOf("x").Convert(ft)
	case reflect.Interface:
		stub, ok := interfaceStubs[ft]
		if !ok {
			t.Fatalf("agent.Runner.%s has interface type %s with no stub here: add one to interfaceStubs, and copy the field in acp.New", name, ft)
		}
		return reflect.ValueOf(stub)
	}
	t.Fatalf("agent.Runner.%s has type %s, which this test cannot fill: extend nonZero", name, ft)
	return reflect.Value{}
}

// The editor's runner keeps every setting of the base runner. A field
// added to agent.Runner later makes this fail until acp.New copies it, or
// it is added, with a reason, to editorOwn.
func TestEditorRunnerKeepsTheBaseRunnersSettings(t *testing.T) {
	base := &agent.Runner{}
	bv := reflect.ValueOf(base).Elem()
	rt := bv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		bv.Field(i).Set(nonZero(t, f.Name, f.Type))
	}
	a, err := New(base, tools.NewRegistry(), "habakkuk", "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ev := reflect.ValueOf(a.runner).Elem()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() || editorOwn[f.Name] {
			continue
		}
		got, want := ev.Field(i), bv.Field(i)
		switch {
		case got.IsZero():
			t.Errorf("agent.Runner.%s is set on the base runner but not on the editor's (copy it in acp.New)", f.Name)
		case f.Type.Kind() != reflect.Func && f.Type.Comparable() && !got.Equal(want):
			t.Errorf("agent.Runner.%s on the editor's runner differs from the base runner's", f.Name)
		}
	}
	// The ones it sets itself are set too.
	if a.runner.Env == nil || len(a.runner.Agents) == 0 {
		t.Fatal("the editor's runner has no Env or agents")
	}
}

// editorWraps are the function fields the editor's runner may wrap rather
// than copy, as long as the wrapper still calls the base runner's own.
// None may be replaced outright.
var editorWraps = map[string]bool{
	"Env": true, // a.env calls the base Env, then points it at the editor's folder
}

// A function field copied as a different function (a stub, a default, a
// wrapper that never calls the base) would pass the not-zero check above.
// So every function field of the base runner is a sentinel that records
// its call, and each one on the editor's runner must reach it.
func TestEditorRunnerCallsTheBaseRunnersFunctions(t *testing.T) {
	base := &agent.Runner{}
	bv := reflect.ValueOf(base).Elem()
	rt := bv.Type()
	called := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Type.Kind() != reflect.Func {
			bv.Field(i).Set(nonZero(t, f.Name, f.Type))
			continue
		}
		name, ft := f.Name, f.Type
		bv.Field(i).Set(reflect.MakeFunc(ft, func([]reflect.Value) []reflect.Value {
			called[name] = true
			out := make([]reflect.Value, ft.NumOut())
			for j := range out {
				out[j] = reflect.Zero(ft.Out(j))
			}
			return out
		}))
	}
	a, err := New(base, tools.NewRegistry(), "habakkuk", "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ev := reflect.ValueOf(a.runner).Elem()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() || f.Type.Kind() != reflect.Func {
			continue
		}
		fn := ev.Field(i)
		if fn.IsNil() {
			t.Errorf("agent.Runner.%s is not set on the editor's runner", f.Name)
			continue
		}
		args := make([]reflect.Value, f.Type.NumIn())
		for j := range args {
			args[j] = reflect.Zero(f.Type.In(j))
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("calling the editor's %s panicked: %v", f.Name, r)
				}
			}()
			fn.Call(args)
		}()
		if !called[f.Name] {
			if editorWraps[f.Name] {
				t.Errorf("the editor's %s is listed as a wrapper but does not call the base runner's", f.Name)
			} else {
				t.Errorf("agent.Runner.%s on the editor's runner is not the base runner's function (copy it in acp.New, or list it in editorWraps with a reason)", f.Name)
			}
		}
	}
}
