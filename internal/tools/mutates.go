package tools

import "encoding/json"

// The file-writing built-ins change local files: they are checkpointed when
// the file is in the workspace, and refused when commands run remotely.

func (writeFile) WritesLocalFiles() {}
func (editFile) WritesLocalFiles()  {}

func (w writeFile) Mutates(env *Env, input json.RawMessage) bool {
	p, err := w.TargetPath(input)
	return err == nil && env.InWorkspace(env.Resolve(p))
}

func (e editFile) Mutates(env *Env, input json.RawMessage) bool {
	p, err := e.TargetPath(input)
	return err == nil && env.InWorkspace(env.Resolve(p))
}
