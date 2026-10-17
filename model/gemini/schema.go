package gemini

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/genai"
)

// maxSchemaDepth limits nesting and $ref expansion, so a cyclic reference
// ends.
const maxSchemaDepth = 32

// ConvertSchema converts a JSON schema to the OpenAPI subset that the SDK
// Schema type holds. It returns nil for an empty schema.
//
// It keeps the types object, string, number, integer, boolean, and array,
// with properties, required, items, description, and enum. A type list or an
// anyOf or oneOf with one type besides "null" becomes that type, marked
// nullable. A local $ref and a one-element allOf resolve to their target.
// An enum on a number or an integer moves into the description, because the
// API takes enum values only as strings. Everything else, such as an object
// with no properties, a union of types, or an unresolved $ref, becomes a
// string with the description.
func ConvertSchema(raw json.RawMessage) (*genai.Schema, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("input schema is not a JSON object: %w", err)
	}
	c := converter{root: root}
	return c.convert(root, 0), nil
}

type converter struct {
	root map[string]any
}

func (c converter) convert(s map[string]any, depth int) *genai.Schema {
	desc, _ := s["description"].(string)
	if depth > maxSchemaDepth {
		return &genai.Schema{Type: genai.TypeString, Description: desc}
	}
	if ref, ok := s["$ref"].(string); ok {
		target := c.resolve(ref)
		if target == nil {
			return &genai.Schema{Type: genai.TypeString, Description: desc}
		}
		return withDescription(c.convert(target, depth+1), desc)
	}
	if all, ok := s["allOf"].([]any); ok {
		if m, ok := single(all); ok {
			return withDescription(c.convert(m, depth+1), desc)
		}
		return &genai.Schema{Type: genai.TypeString, Description: desc}
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		alts, ok := s[key].([]any)
		if !ok {
			continue
		}
		var (
			rest     []map[string]any
			nullable bool
		)
		for _, a := range alts {
			m, ok := a.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t == "null" {
				nullable = true
				continue
			}
			rest = append(rest, m)
		}
		if len(rest) != 1 {
			return &genai.Schema{Type: genai.TypeString, Description: desc}
		}
		out := withDescription(c.convert(rest[0], depth+1), desc)
		if nullable {
			out.Nullable = genai.Ptr(true)
		}
		return out
	}

	typ, nullable := schemaType(s)
	out := &genai.Schema{Description: desc}
	if nullable {
		out.Nullable = genai.Ptr(true)
	}
	switch typ {
	case "object":
		props, _ := s["properties"].(map[string]any)
		if len(props) == 0 {
			if depth == 0 {
				// The caller sends a tool with no parameters.
				out.Type = genai.TypeObject
				return out
			}
			out.Type = genai.TypeString
			return out
		}
		out.Type = genai.TypeObject
		out.Properties = make(map[string]*genai.Schema, len(props))
		for name, p := range props {
			m, _ := p.(map[string]any)
			if m == nil {
				m = map[string]any{}
			}
			out.Properties[name] = c.convert(m, depth+1)
		}
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if name, ok := r.(string); ok && props[name] != nil {
					out.Required = append(out.Required, name)
				}
			}
		}
	case "array":
		out.Type = genai.TypeArray
		if items, ok := s["items"].(map[string]any); ok {
			out.Items = c.convert(items, depth+1)
		} else {
			out.Items = &genai.Schema{Type: genai.TypeString}
		}
	case "string":
		out.Type = genai.TypeString
		if values := enumOf(s); len(values) > 0 {
			out.Format = "enum"
			out.Enum = values
		} else if f, _ := s["format"].(string); f == "date-time" {
			// The API takes no other string format.
			out.Format = f
		}
	case "integer", "number":
		out.Type = genai.TypeNumber
		if typ == "integer" {
			out.Type = genai.TypeInteger
		}
		if values := enumOf(s); len(values) > 0 {
			out.Description = strings.TrimSpace(desc + " Allowed values: " + strings.Join(values, ", ") + ".")
		}
	case "boolean":
		out.Type = genai.TypeBoolean
	default:
		out.Type = genai.TypeString
	}
	return out
}

// schemaType returns the type of a schema and whether it allows null. A type
// list with more than one type besides "null" returns "". A schema with no
// type takes its type from properties, items, enum, or const.
func schemaType(s map[string]any) (string, bool) {
	switch t := s["type"].(type) {
	case string:
		return t, false
	case []any:
		var (
			typ      string
			nullable bool
		)
		for _, x := range t {
			name, _ := x.(string)
			switch {
			case name == "null":
				nullable = true
			case typ == "":
				typ = name
			default:
				return "", nullable
			}
		}
		return typ, nullable
	}
	switch {
	case s["properties"] != nil:
		return "object", false
	case s["items"] != nil:
		return "array", false
	case s["enum"] != nil, s["const"] != nil:
		return "string", false
	}
	return "", false
}

// enumOf returns the enum, or the const, of a schema as strings.
func enumOf(s map[string]any) []string {
	values, _ := s["enum"].([]any)
	if c, ok := s["const"]; ok && values == nil {
		values = []any{c}
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case float64:
			out = append(out, strconv.FormatFloat(x, 'f', -1, 64))
		case nil:
			// null in an enum marks the value optional; the API has no null value.
		default:
			out = append(out, fmt.Sprint(x))
		}
	}
	return out
}

// resolve follows a local reference such as "#/$defs/Item". It returns nil
// for a reference outside the document or a path that does not exist.
func (c converter) resolve(ref string) map[string]any {
	if ref == "#" {
		return c.root
	}
	rest, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return nil
	}
	var cur any = c.root
	for _, tok := range strings.Split(rest, "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[tok]
	}
	m, _ := cur.(map[string]any)
	return m
}

// single returns the only schema of a list.
func single(list []any) (map[string]any, bool) {
	if len(list) != 1 {
		return nil, false
	}
	m, ok := list[0].(map[string]any)
	return m, ok
}

// withDescription sets the description of s when desc is not empty.
func withDescription(s *genai.Schema, desc string) *genai.Schema {
	if desc != "" {
		s.Description = desc
	}
	return s
}
