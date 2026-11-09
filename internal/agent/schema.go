package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/habakkuk-ai/habakkuk/internal/core/jobs"
	"github.com/habakkuk-ai/habakkuk/internal/llm"
)

// A small JSON Schema validator for delegate's output_schema. It covers the
// keywords models are asked to follow: type, enum, const, properties,
// required, additionalProperties, items, prefixItems, min/maxItems,
// uniqueItems, min/maxLength, pattern, minimum, maximum, exclusive bounds,
// multipleOf, min/maxProperties, allOf, anyOf, oneOf, not, and $ref to
// local $defs or definitions. Unknown keywords (format, description) are
// ignored, as the specification allows.

type schema struct {
	root *schemaNode
}

type schemaNode struct {
	Bool *bool // a boolean schema: true accepts anything, false nothing

	Type        []string
	Enum        []any
	Const       *any
	Properties  map[string]*schemaNode
	Required    []string
	AddlProps   *schemaNode
	Items       *schemaNode
	PrefixItems []*schemaNode
	MinItems    *int
	MaxItems    *int
	Unique      bool
	MinLength   *int
	MaxLength   *int
	Pattern     *regexp.Regexp
	Minimum     *float64
	Maximum     *float64
	ExclMin     *float64
	ExclMax     *float64
	MultipleOf  *float64
	MinProps    *int
	MaxProps    *int
	AllOf       []*schemaNode
	AnyOf       []*schemaNode
	OneOf       []*schemaNode
	Not         *schemaNode
	Ref         string
	Defs        map[string]*schemaNode
}

// parseSchema reads a JSON Schema document.
func parseSchema(raw json.RawMessage) (*schema, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	n, err := compileNode(v)
	if err != nil {
		return nil, err
	}
	return &schema{root: n}, nil
}

func compileNode(v any) (*schemaNode, error) {
	switch s := v.(type) {
	case bool:
		return &schemaNode{Bool: &s}, nil
	case map[string]any:
		return compileObject(s)
	}
	return nil, errors.New("a schema must be an object or a boolean")
}

func compileObject(m map[string]any) (*schemaNode, error) {
	n := &schemaNode{}
	var err error
	sub := func(key string) (*schemaNode, error) {
		if v, ok := m[key]; ok {
			return compileNode(v)
		}
		return nil, nil
	}
	list := func(key string) ([]*schemaNode, error) {
		v, ok := m[key]
		if !ok {
			return nil, nil
		}
		arr, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of schemas", key)
		}
		out := make([]*schemaNode, len(arr))
		for i, x := range arr {
			if out[i], err = compileNode(x); err != nil {
				return nil, fmt.Errorf("%s[%d]: %w", key, i, err)
			}
		}
		return out, nil
	}
	num := func(key string) (*float64, error) {
		v, ok := m[key]
		if !ok {
			return nil, nil
		}
		f, ok := toFloat(v)
		if !ok {
			return nil, fmt.Errorf("%s must be a number", key)
		}
		return &f, nil
	}
	count := func(key string) (*int, error) {
		f, err := num(key)
		if err != nil || f == nil {
			return nil, err
		}
		if *f < 0 || *f != math.Trunc(*f) {
			return nil, fmt.Errorf("%s must be a non-negative integer", key)
		}
		i := int(*f)
		return &i, nil
	}
	switch t := m["type"].(type) {
	case nil:
	case string:
		n.Type = []string{t}
	case []any:
		for _, x := range t {
			s, ok := x.(string)
			if !ok {
				return nil, errors.New("type must be a string or an array of strings")
			}
			n.Type = append(n.Type, s)
		}
	default:
		return nil, errors.New("type must be a string or an array of strings")
	}
	for _, t := range n.Type {
		if !slices.Contains([]string{"object", "array", "string", "number", "integer", "boolean", "null"}, t) {
			return nil, fmt.Errorf("unknown type %q", t)
		}
	}
	if e, ok := m["enum"]; ok {
		arr, ok := e.([]any)
		if !ok {
			return nil, errors.New("enum must be an array")
		}
		n.Enum = arr
	}
	if c, ok := m["const"]; ok {
		n.Const = &c
	}
	if p, ok := m["properties"]; ok {
		pm, ok := p.(map[string]any)
		if !ok {
			return nil, errors.New("properties must be an object")
		}
		n.Properties = map[string]*schemaNode{}
		for k, v := range pm {
			if n.Properties[k], err = compileNode(v); err != nil {
				return nil, fmt.Errorf("properties.%s: %w", k, err)
			}
		}
	}
	if r, ok := m["required"]; ok {
		arr, ok := r.([]any)
		if !ok {
			return nil, errors.New("required must be an array of names")
		}
		for _, x := range arr {
			s, ok := x.(string)
			if !ok {
				return nil, errors.New("required must be an array of names")
			}
			n.Required = append(n.Required, s)
		}
	}
	if n.AddlProps, err = sub("additionalProperties"); err != nil {
		return nil, fmt.Errorf("additionalProperties: %w", err)
	}
	if it, ok := m["items"]; ok {
		if _, isArr := it.([]any); isArr { // the older tuple form
			if n.PrefixItems, err = list("items"); err != nil {
				return nil, err
			}
		} else if n.Items, err = compileNode(it); err != nil {
			return nil, fmt.Errorf("items: %w", err)
		}
	}
	if pi, err := list("prefixItems"); err != nil {
		return nil, err
	} else if pi != nil {
		n.PrefixItems = pi
	}
	if n.MinItems, err = count("minItems"); err != nil {
		return nil, err
	}
	if n.MaxItems, err = count("maxItems"); err != nil {
		return nil, err
	}
	n.Unique, _ = m["uniqueItems"].(bool)
	if n.MinLength, err = count("minLength"); err != nil {
		return nil, err
	}
	if n.MaxLength, err = count("maxLength"); err != nil {
		return nil, err
	}
	if p, ok := m["pattern"].(string); ok {
		if n.Pattern, err = regexp.Compile(p); err != nil {
			return nil, fmt.Errorf("pattern: %w", err)
		}
	}
	for key, dst := range map[string]**float64{"minimum": &n.Minimum, "maximum": &n.Maximum,
		"exclusiveMinimum": &n.ExclMin, "exclusiveMaximum": &n.ExclMax, "multipleOf": &n.MultipleOf} {
		if *dst, err = num(key); err != nil {
			return nil, err
		}
	}
	if n.MultipleOf != nil && *n.MultipleOf <= 0 {
		return nil, errors.New("multipleOf must be greater than 0")
	}
	if n.MinProps, err = count("minProperties"); err != nil {
		return nil, err
	}
	if n.MaxProps, err = count("maxProperties"); err != nil {
		return nil, err
	}
	if n.AllOf, err = list("allOf"); err != nil {
		return nil, err
	}
	if n.AnyOf, err = list("anyOf"); err != nil {
		return nil, err
	}
	if n.OneOf, err = list("oneOf"); err != nil {
		return nil, err
	}
	if n.Not, err = sub("not"); err != nil {
		return nil, fmt.Errorf("not: %w", err)
	}
	n.Ref, _ = m["$ref"].(string)
	for _, key := range []string{"$defs", "definitions"} {
		d, ok := m[key].(map[string]any)
		if !ok {
			continue
		}
		if n.Defs == nil {
			n.Defs = map[string]*schemaNode{}
		}
		for k, v := range d {
			node, err := compileNode(v)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", key, k, err)
			}
			n.Defs["#/"+key+"/"+k] = node
		}
	}
	return n, nil
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	}
	return 0, false
}

// validate checks a decoded JSON value (numbers as json.Number) and returns
// the first few problems, each with its path.
func (s *schema) validate(v any) error {
	var errs []string
	s.check(s.root, v, "$", &errs, 0)
	if len(errs) == 0 {
		return nil
	}
	if len(errs) > 5 {
		errs = append(errs[:5], fmt.Sprintf("and %d more", len(errs)-5))
	}
	return errors.New(strings.Join(errs, "; "))
}

func (s *schema) check(n *schemaNode, v any, path string, errs *[]string, depth int) {
	bad := func(format string, args ...any) {
		*errs = append(*errs, path+": "+fmt.Sprintf(format, args...))
	}
	if depth > 64 {
		bad("schema nests too deep (a $ref loop?)")
		return
	}
	if n.Bool != nil {
		if !*n.Bool {
			bad("no value is allowed here")
		}
		return
	}
	if n.Ref != "" {
		target := s.root.Defs[n.Ref]
		if n.Ref == "#" {
			target = s.root
		}
		if target == nil {
			bad("unresolvable $ref %q", n.Ref)
			return
		}
		s.check(target, v, path, errs, depth+1)
	}
	if len(n.Type) > 0 && !slices.ContainsFunc(n.Type, func(t string) bool { return hasType(v, t) }) {
		bad("expected %s, got %s", strings.Join(n.Type, " or "), typeName(v))
		return
	}
	if n.Enum != nil && !slices.ContainsFunc(n.Enum, func(e any) bool { return equalJSON(e, v) }) {
		bad("must be one of %s", compact(n.Enum))
	}
	if n.Const != nil && !equalJSON(*n.Const, v) {
		bad("must be %s", compact(*n.Const))
	}
	switch x := v.(type) {
	case map[string]any:
		for _, k := range n.Required {
			if _, ok := x[k]; !ok {
				bad("missing required property %q", k)
			}
		}
		if n.MinProps != nil && len(x) < *n.MinProps {
			bad("needs at least %d properties", *n.MinProps)
		}
		if n.MaxProps != nil && len(x) > *n.MaxProps {
			bad("allows at most %d properties", *n.MaxProps)
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if p, ok := n.Properties[k]; ok {
				s.check(p, x[k], path+"."+k, errs, depth+1)
			} else if n.AddlProps != nil {
				if n.AddlProps.Bool != nil && !*n.AddlProps.Bool {
					bad("property %q is not allowed", k)
				} else {
					s.check(n.AddlProps, x[k], path+"."+k, errs, depth+1)
				}
			}
		}
	case []any:
		if n.MinItems != nil && len(x) < *n.MinItems {
			bad("needs at least %d items, has %d", *n.MinItems, len(x))
		}
		if n.MaxItems != nil && len(x) > *n.MaxItems {
			bad("allows at most %d items, has %d", *n.MaxItems, len(x))
		}
		for i, item := range x {
			p := fmt.Sprintf("%s[%d]", path, i)
			switch {
			case i < len(n.PrefixItems):
				s.check(n.PrefixItems[i], item, p, errs, depth+1)
			case n.Items != nil:
				s.check(n.Items, item, p, errs, depth+1)
			}
		}
		if n.Unique {
			for i := range x {
				for j := i + 1; j < len(x); j++ {
					if equalJSON(x[i], x[j]) {
						bad("items %d and %d are the same", i, j)
					}
				}
			}
		}
	case string:
		l := utf8.RuneCountInString(x)
		if n.MinLength != nil && l < *n.MinLength {
			bad("must be at least %d characters", *n.MinLength)
		}
		if n.MaxLength != nil && l > *n.MaxLength {
			bad("must be at most %d characters", *n.MaxLength)
		}
		if n.Pattern != nil && !n.Pattern.MatchString(x) {
			bad("must match %s", n.Pattern)
		}
	case json.Number:
		f, _ := x.Float64()
		switch {
		case n.Minimum != nil && f < *n.Minimum:
			bad("must be at least %v", *n.Minimum)
		case n.Maximum != nil && f > *n.Maximum:
			bad("must be at most %v", *n.Maximum)
		case n.ExclMin != nil && f <= *n.ExclMin:
			bad("must be more than %v", *n.ExclMin)
		case n.ExclMax != nil && f >= *n.ExclMax:
			bad("must be less than %v", *n.ExclMax)
		}
		if n.MultipleOf != nil {
			if q := f / *n.MultipleOf; math.Abs(q-math.Round(q)) > 1e-9 {
				bad("must be a multiple of %v", *n.MultipleOf)
			}
		}
	}
	for _, a := range n.AllOf {
		s.check(a, v, path, errs, depth+1)
	}
	if len(n.AnyOf) > 0 && !slices.ContainsFunc(n.AnyOf, func(a *schemaNode) bool { return s.matches(a, v, depth) }) {
		bad("matches none of anyOf")
	}
	if len(n.OneOf) > 0 {
		hits := 0
		for _, a := range n.OneOf {
			if s.matches(a, v, depth) {
				hits++
			}
		}
		if hits != 1 {
			bad("must match exactly one of oneOf, matches %d", hits)
		}
	}
	if n.Not != nil && s.matches(n.Not, v, depth) {
		bad("must not match the schema in not")
	}
}

func (s *schema) matches(n *schemaNode, v any, depth int) bool {
	var errs []string
	s.check(n, v, "$", &errs, depth+1)
	return len(errs) == 0
}

func hasType(v any, t string) bool {
	switch x := v.(type) {
	case nil:
		return t == "null"
	case bool:
		return t == "boolean"
	case string:
		return t == "string"
	case []any:
		return t == "array"
	case map[string]any:
		return t == "object"
	case json.Number:
		if t == "number" {
			return true
		}
		if t == "integer" {
			f, err := x.Float64()
			return err == nil && f == math.Trunc(f)
		}
	}
	return false
}

func typeName(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case json.Number:
		if hasType(x, "integer") {
			return "integer"
		}
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func equalJSON(a, b any) bool {
	an, aok := toFloat(a)
	bn, bok := toFloat(b)
	if aok && bok {
		return an == bn
	}
	ja, _ := json.Marshal(normalize(a))
	jb, _ := json.Marshal(normalize(b))
	return bytes.Equal(ja, jb)
}

// normalize turns numbers into float64 so 1 and 1.0 compare equal inside
// objects and arrays (json.Marshal sorts map keys).
func normalize(v any) any {
	switch x := v.(type) {
	case json.Number:
		f, _ := x.Float64()
		return f
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalize(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normalize(e)
		}
		return out
	}
	return v
}

func compact(v any) string {
	b, _ := json.Marshal(v)
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

// finalJSON finds the JSON value an answer ends with: the whole answer, the
// last fenced block, or the outermost value that runs to the end of the
// text (ignoring a closing fence). It returns the value's text.
func finalJSON(answer string) (string, any, error) {
	s := strings.TrimSpace(answer)
	try := func(t string) (any, bool) {
		dec := json.NewDecoder(strings.NewReader(t))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil {
			return nil, false
		}
		rest := strings.TrimSpace(t[dec.InputOffset():])
		return v, rest == "" || rest == "```"
	}
	if v, ok := try(s); ok {
		return strings.TrimSuffix(strings.TrimSpace(s), "```"), v, nil
	}
	if i := strings.LastIndex(s, "```"); i >= 0 && strings.TrimSpace(s[i+3:]) == "" {
		body := s[:i]
		if j := strings.LastIndex(body, "```"); j >= 0 {
			block := body[j+3:]
			if nl := strings.IndexByte(block, '\n'); nl >= 0 && !strings.ContainsAny(block[:nl], "{[\"") {
				block = block[nl+1:] // a language tag such as json
			}
			if v, ok := try(block); ok {
				return strings.TrimSpace(block), v, nil
			}
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '{' && s[i] != '[' {
			continue
		}
		if v, ok := try(s[i:]); ok {
			return strings.TrimSuffix(strings.TrimSpace(s[i:]), "```"), v, nil
		}
	}
	return "", nil, errors.New("the answer does not end with a JSON value")
}

// checkAnswer extracts an answer's final JSON and validates it.
func checkAnswer(answer string, raw json.RawMessage) (string, error) {
	sch, err := parseSchema(raw)
	if err != nil {
		return "", fmt.Errorf("output_schema: %w", err)
	}
	text, v, err := finalJSON(answer)
	if err != nil {
		return "", err
	}
	if err := sch.validate(v); err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

// schemaRetryPrefix starts the message asking a sub-agent to fix its answer.
const schemaRetryPrefix = "[Your final answer does not match the required JSON Schema]"

// enforceSchema holds a sub-turn's answer to its output schema. A mismatch
// gets one more try in the same turn, with the validation error; a second
// mismatch fails the sub-turn. The retry is a message in the transcript, so
// a rerun after a crash knows it was already asked.
func (t *turn) enforceSchema(task string, raw json.RawMessage) error {
	out, verr := checkAnswer(t.final, raw)
	if verr == nil {
		t.final = out
		return nil
	}
	for _, m := range t.msgs {
		if m.Role == llm.User && strings.HasPrefix(m.Text(), schemaRetryPrefix) {
			return jobs.Permanent{Err: fmt.Errorf("the sub-agent's answer does not match output_schema: %v", verr)}
		}
	}
	msg := fmt.Sprintf("%s\n%v\n\nReply again with only the corrected JSON value, matching:\n```json\n%s\n```", schemaRetryPrefix, verr, raw)
	if err := t.append(llm.UserText(msg), "", nil); err != nil {
		return err
	}
	again := &turn{r: t.r, ctx: t.ctx, job: t.job, owner: t.owner, sess: t.sess, spec: t.spec, chain: t.chain, sub: t.sub}
	if err := again.run(task, nil); err != nil {
		return err
	}
	t.msgs, t.final = again.msgs, again.final
	out, verr = checkAnswer(t.final, raw)
	if verr != nil {
		return jobs.Permanent{Err: fmt.Errorf("the sub-agent's answer does not match output_schema after one retry: %v", verr)}
	}
	t.final = out
	return nil
}
