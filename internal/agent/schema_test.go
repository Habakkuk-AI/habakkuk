package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSchemaValidation(t *testing.T) {
	sch := `{
		"type":"object",
		"required":["name","tags","score"],
		"additionalProperties":false,
		"properties":{
			"name":{"type":"string","minLength":2,"pattern":"^[a-z]+$"},
			"tags":{"type":"array","items":{"enum":["a","b"]},"uniqueItems":true,"maxItems":2},
			"score":{"type":"integer","minimum":0,"maximum":10},
			"kind":{"$ref":"#/$defs/kind"},
			"either":{"anyOf":[{"type":"string"},{"type":"null"}]}
		},
		"$defs":{"kind":{"const":"x"}}
	}`
	for _, c := range []struct {
		doc  string
		want string // a fragment of the error, "" for valid
	}{
		{`{"name":"ab","tags":["a"],"score":3}`, ""},
		{`{"name":"ab","tags":[],"score":3.0,"kind":"x","either":null}`, ""},
		{`{"name":"a","tags":[],"score":3}`, "$.name: must be at least 2"},
		{`{"name":"AB","tags":[],"score":3}`, "must match"},
		{`{"name":"ab","tags":["c"],"score":3}`, `$.tags[0]: must be one of`},
		{`{"name":"ab","tags":["a","a"],"score":3}`, "are the same"},
		{`{"name":"ab","tags":[],"score":3.5}`, "expected integer"},
		{`{"name":"ab","tags":[],"score":11}`, "at most 10"},
		{`{"name":"ab","tags":[]}`, `missing required property "score"`},
		{`{"name":"ab","tags":[],"score":1,"extra":1}`, `"extra" is not allowed`},
		{`{"name":"ab","tags":[],"score":1,"kind":"y"}`, `$.kind: must be "x"`},
		{`{"name":"ab","tags":[],"score":1,"either":5}`, "none of anyOf"},
		{`[1]`, "expected object, got array"},
	} {
		_, err := checkAnswer(c.doc, json.RawMessage(sch))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.doc, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error %v, want %q", c.doc, err, c.want)
		}
	}
	if _, err := parseSchema(json.RawMessage(`{"type":"thing"}`)); err == nil {
		t.Error("unknown type accepted")
	}
	if _, err := parseSchema(json.RawMessage(`[1]`)); err == nil {
		t.Error("an array accepted as a schema")
	}
}

func TestFinalJSON(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":1}`:                                `{"a":1}`,
		"Done.\n```json\n{\"a\": [1, 2]}\n```":   `{"a": [1, 2]}`,
		"I found [1,2] items. Answer: {\"b\":2}": `{"b":2}`,
		"Answer:\n[\"x\", {\"y\": 1}]\n":         `["x", {"y": 1}]`,
	} {
		got, _, err := finalJSON(in)
		if err != nil || got != want {
			t.Errorf("finalJSON(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, _, err := finalJSON(`{"a":1} and then some prose`); err == nil {
		t.Error("JSON followed by prose accepted")
	}
}
