package gemini

import (
	"encoding/json"
	"reflect"
	"testing"
)

// jsonEqual reports whether two JSON documents hold the same value.
func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v\n%s", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestConvertSchema(t *testing.T) {
	in := `{
	  "type": "object",
	  "description": "Search input",
	  "additionalProperties": false,
	  "properties": {
	    "query": {"type": "string", "description": "What to find", "minLength": 1},
	    "mode": {"type": "string", "enum": ["fast", "deep"]},
	    "when": {"type": "string", "format": "date-time"},
	    "uri": {"type": "string", "format": "uri"},
	    "limit": {"type": "integer", "description": "How many", "enum": [10, 20]},
	    "score": {"type": "number"},
	    "exact": {"type": "boolean"},
	    "tags": {"type": "array", "items": {"type": "string"}},
	    "bare": {"type": "array"},
	    "filter": {
	      "type": "object",
	      "properties": {"field": {"type": "string"}, "value": {"type": ["string", "null"]}},
	      "required": ["field", "missing"]
	    },
	    "meta": {"type": "object", "description": "Free-form map", "additionalProperties": true},
	    "either": {"anyOf": [{"type": "string"}, {"type": "integer"}], "description": "A union"},
	    "maybe": {"anyOf": [{"type": "integer"}, {"type": "null"}], "description": "Optional count"},
	    "item": {"$ref": "#/$defs/Item"},
	    "wrapped": {"allOf": [{"$ref": "#/$defs/Item"}], "description": "Wrapped item"},
	    "far": {"$ref": "https://example.com/schema.json", "description": "Remote"},
	    "anything": {"description": "No type"}
	  },
	  "required": ["query"],
	  "$defs": {"Item": {"type": "object", "properties": {"id": {"type": "integer"}}, "required": ["id"]}}
	}`
	s, err := ConvertSchema(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, got, `{
	  "type": "OBJECT",
	  "description": "Search input",
	  "properties": {
	    "query": {"type": "STRING", "description": "What to find"},
	    "mode": {"type": "STRING", "format": "enum", "enum": ["fast", "deep"]},
	    "when": {"type": "STRING", "format": "date-time"},
	    "uri": {"type": "STRING"},
	    "limit": {"type": "INTEGER", "description": "How many Allowed values: 10, 20."},
	    "score": {"type": "NUMBER"},
	    "exact": {"type": "BOOLEAN"},
	    "tags": {"type": "ARRAY", "items": {"type": "STRING"}},
	    "bare": {"type": "ARRAY", "items": {"type": "STRING"}},
	    "filter": {
	      "type": "OBJECT",
	      "properties": {"field": {"type": "STRING"}, "value": {"type": "STRING", "nullable": true}},
	      "required": ["field"]
	    },
	    "meta": {"type": "STRING", "description": "Free-form map"},
	    "either": {"type": "STRING", "description": "A union"},
	    "maybe": {"type": "INTEGER", "description": "Optional count", "nullable": true},
	    "item": {"type": "OBJECT", "properties": {"id": {"type": "INTEGER"}}, "required": ["id"]},
	    "wrapped": {"type": "OBJECT", "description": "Wrapped item", "properties": {"id": {"type": "INTEGER"}}, "required": ["id"]},
	    "far": {"type": "STRING", "description": "Remote"},
	    "anything": {"type": "STRING", "description": "No type"}
	  },
	  "required": ["query"]
	}`)
}

func TestConvertSchemaCycle(t *testing.T) {
	in := `{"type":"object","properties":{"node":{"$ref":"#/$defs/Node"}},
	  "$defs":{"Node":{"type":"object","properties":{"next":{"$ref":"#/$defs/Node"},"v":{"type":"string"}}}}}`
	s, err := ConvertSchema(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	depth := 0
	for n := s.Properties["node"]; n != nil && n.Properties != nil; n = n.Properties["next"] {
		depth++
	}
	if depth == 0 || depth > maxSchemaDepth {
		t.Fatalf("cyclic reference expanded %d levels, want 1..%d", depth, maxSchemaDepth)
	}
}

func TestConvertSchemaErrors(t *testing.T) {
	if s, err := ConvertSchema(nil); s != nil || err != nil {
		t.Errorf("empty schema = %v, %v; want nil, nil", s, err)
	}
	if _, err := ConvertSchema(json.RawMessage(`[1]`)); err == nil {
		t.Error("a schema that is not an object should fail")
	}
}
