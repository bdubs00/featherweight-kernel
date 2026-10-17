package config

// This file expands variables in a configuration file. A value in fw.yaml
// or in a profile file reads ${NAME} and ${NAME:-default} from the
// environment before it is decoded, so a file names an endpoint by its
// variable instead of by a literal. The expansion works on the YAML node
// tree: keys stay as they are, and the expanded text is read as if it was
// written in the file, so a plain ${PORT} that expands to 8080 is a number
// and a quoted "${PORT}" is a string.

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

// decodeYAML overlays a YAML document on v, which is a pointer to a struct.
// Every scalar value expands its variables through lookup first. A key that
// the struct does not know is an error that names the key and its line.
func decodeYAML(data []byte, v any, lookup lookupFunc) error {
	var n yaml.Node
	if err := yaml.Unmarshal(data, &n); err != nil {
		return err
	}
	if n.Kind == 0 {
		return nil // an empty file changes nothing
	}
	var errs []string
	checkKeys(&n, reflect.TypeOf(v).Elem(), &errs)
	expandNode(&n, "", lookup, &errs)
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	err := n.Decode(v)
	var te *yaml.TypeError
	if errors.As(err, &te) {
		return errors.New(strings.Join(te.Errors, "; "))
	}
	return err
}

// expandNode expands the scalar values under n in place. path names the
// position of a value for an error, such as tools.mcp[0].endpoint.
func expandNode(n *yaml.Node, path string, lookup lookupFunc, errs *[]string) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			expandNode(c, path, lookup, errs)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			expandNode(c, fmt.Sprintf("%s[%d]", path, i), lookup, errs)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			p := n.Content[i].Value
			if path != "" {
				p = path + "." + p
			}
			expandNode(n.Content[i+1], p, lookup, errs)
		}
	case yaml.ScalarNode:
		// An alias points at a node that the walk reaches where its anchor
		// is, so an alias is not expanded again.
		if !strings.Contains(n.Value, "$") {
			return
		}
		v, err := expand(n.Value, lookup)
		if err != nil {
			*errs = append(*errs, fmt.Sprintf("line %d: %s: %v", n.Line, path, err))
			return
		}
		n.Value = v
		if n.Style == 0 {
			// A plain scalar takes the type of its new text: 8080 is a
			// number and true is a boolean. A quoted scalar stays a string.
			n.Tag = ""
		}
	}
}

// expand replaces ${NAME} with the value of the variable and
// ${NAME:-default} with the value, or with the default when the variable
// is unset or empty. $$ is one $. A default can hold a reference itself.
// Any other $ stays as it is, so a price such as $5 needs no escape.
func expand(s string, lookup lookupFunc) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			b.WriteByte('$')
			i++
			continue
		}
		if i+1 >= len(s) || s[i+1] != '{' {
			b.WriteByte('$')
			continue
		}
		end := closingBrace(s, i+1)
		if end < 0 {
			return "", fmt.Errorf("%s has no closing brace", s[i:])
		}
		ref := s[i+2 : end]
		name, def, hasDefault := strings.Cut(ref, ":-")
		if !validVarName(name) {
			return "", fmt.Errorf("${%s} is not a variable reference; a name holds letters, digits, and underscores", ref)
		}
		v, ok := lookup(name)
		switch {
		case ok && v != "", ok && !hasDefault:
		case hasDefault:
			d, err := expand(def, lookup)
			if err != nil {
				return "", err
			}
			v = d
		default:
			return "", fmt.Errorf("variable %s is not set; an empty value counts as unset", name)
		}
		b.WriteString(v)
		i = end
	}
	return b.String(), nil
}

// closingBrace returns the index of the brace that closes the one at
// open, or -1. Braces inside the reference nest.
func closingBrace(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// validVarName reports whether s is a variable name: a letter or an
// underscore, then letters, digits, and underscores.
func validVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		letter := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		digit := r >= '0' && r <= '9'
		if !letter && !(digit && i > 0) {
			return false
		}
	}
	return true
}

// checkKeys adds one error per mapping key under n that the type t has no
// field for, in the words of the yaml package, with the line of the key.
// The yaml package checks the keys when it decodes bytes but not when it
// decodes a node tree, and the expansion works on the node tree.
func checkKeys(n *yaml.Node, t reflect.Type, errs *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			checkKeys(c, t, errs)
		}
	case yaml.AliasNode:
		checkKeys(n.Alias, t, errs)
	case yaml.SequenceNode:
		if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			for _, c := range n.Content {
				checkKeys(c, t.Elem(), errs)
			}
		}
	case yaml.MappingNode:
		switch t.Kind() {
		case reflect.Map:
			for i := 0; i+1 < len(n.Content); i += 2 {
				checkKeys(n.Content[i+1], t.Elem(), errs)
			}
		case reflect.Struct:
			fields, inlineMap := structKeys(t)
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				if k.Tag == "!!merge" {
					// "<<: *base" or "<<: [*a, *b]" merges mappings of
					// the same type.
					if v.Kind == yaml.SequenceNode {
						for _, c := range v.Content {
							checkKeys(c, t, errs)
						}
					} else {
						checkKeys(v, t, errs)
					}
					continue
				}
				ft, ok := fields[k.Value]
				if !ok {
					// An inline map of yaml.Node holds the extension blocks
					// (fw.Config.Extensions). It takes a claimed key only, and
					// the package that claimed the key checks the block.
					if inlineMap == nil || (inlineMap.Elem() == nodeType && !slices.Contains(fw.RegisteredExtensions(), k.Value)) {
						*errs = append(*errs, fmt.Sprintf("line %d: field %s not found in type %s", k.Line, k.Value, t))
						continue
					}
					if inlineMap.Elem() == nodeType {
						continue
					}
					ft = inlineMap.Elem()
				}
				checkKeys(v, ft, errs)
			}
		}
	}
}

// nodeType is the element type of fw.Config.Extensions.
var nodeType = reflect.TypeOf(yaml.Node{})

// structKeys maps each yaml key of the struct t to the type of its field,
// with the fields of an inline struct in place of the struct. The second
// result is the type of an inline map, which takes every other key, or nil.
func structKeys(t reflect.Type) (map[string]reflect.Type, reflect.Type) {
	out := map[string]reflect.Type{}
	var inlineMap reflect.Type
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		key, inline := yamlKey(f)
		if key == "-" {
			continue
		}
		if !inline {
			out[key] = f.Type
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch ft.Kind() {
		case reflect.Struct:
			sub, m := structKeys(ft)
			for k, v := range sub {
				out[k] = v
			}
			if m != nil {
				inlineMap = m
			}
		case reflect.Map:
			inlineMap = ft
		}
	}
	return out, inlineMap
}
