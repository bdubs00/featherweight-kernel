package config

import (
	"fmt"
	"net/textproto"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// EnvPrefix starts the name of every configuration variable.
const EnvPrefix = "FW"

// namePlaceholder stands for the key or the entry name in a pattern that
// EnvNames lists, such as FW_TOOLS_MCP_<NAME>_ENDPOINT.
const namePlaceholder = "<NAME>"

// envAliases maps a variable to a shorter name that sets the same key. The
// full name wins when both are set.
var envAliases = map[string]string{
	"FW_TELEMETRY_LOG": "FW_LOG",
}

var durationType = reflect.TypeOf(time.Duration(0))

// lookupFunc has the shape of os.LookupEnv.
type lookupFunc func(string) (string, bool)

// environ is the environment as a map. An empty value is not in the map,
// because an empty value counts as unset.
type environ map[string]string

// parseEnviron reads a list of NAME=VALUE pairs, as os.Environ returns.
func parseEnviron(list []string) environ {
	e := environ{}
	for _, kv := range list {
		name, value, ok := strings.Cut(kv, "=")
		if ok && name != "" && strings.TrimSpace(value) != "" {
			e[name] = value
		}
	}
	return e
}

// lookup returns the value of a variable or of its alias.
func (e environ) lookup(name string) (string, bool) {
	if v, ok := e[name]; ok {
		return v, true
	}
	if alias, ok := envAliases[name]; ok {
		if v, ok := e[alias]; ok {
			return v, true
		}
	}
	return "", false
}

// under returns the names of the variables that start with the prefix and
// an underscore, sorted.
func (e environ) under(prefix string) []string {
	var out []string
	for name := range e {
		if strings.HasPrefix(name, prefix+"_") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// applyEnv overlays the FW_* variables on cfg. list is the environment as
// NAME=VALUE pairs, in the shape of os.Environ. The rules come from the yaml
// tags of fw.Config, so a new field needs no code here:
//
//   - A key path maps to FW_ plus each key in upper snake case, joined by
//     underscores: bus.url is FW_BUS_URL and tools.searchThreshold is
//     FW_TOOLS_SEARCH_THRESHOLD. An inline struct adds no key.
//   - Strings, booleans, integers, floats, and durations ("2h") are scalars.
//   - A list of strings is a comma-separated value.
//   - A map of strings takes one variable per key: FW_MODELS_FAST sets
//     models.fast, and FW_TOOLS_BASH_ENV_NATS_URL sets tools.bash.env.NATS_URL.
//     keyRules says how the key segment of the variable becomes the map key.
//   - A map of structs, and a list of structs that have a name field, take
//     one variable per field of one entry: FW_PROVIDERS_OPENAI_BASE_URL sets
//     providers.openai.baseUrl, and FW_TOOLS_MCP_GIT_ENDPOINT sets the
//     endpoint of the tools.mcp entry named git. An entry that the file does
//     not have is created. The entry name ends where the longest known field
//     starts, so FW_TOOLS_MCP_GIT_MCP_ENDPOINT names the entry git_mcp. A
//     variable under such a prefix that ends with no known field is an error
//     that lists the fields.
//   - An empty value counts as unset.
//
// applyEnv returns one error that names every variable it could not apply.
func applyEnv(cfg *fw.Config, list []string) error {
	e := parseEnviron(list)
	var errs []string
	envStruct(reflect.ValueOf(cfg).Elem(), EnvPrefix, "", e, &errs)
	if len(errs) > 0 {
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return nil
}

// envStruct sets the fields of the struct v from the variables under the
// prefix and reports whether it set any. path is the dotted yaml path of
// the struct, for messages.
func envStruct(v reflect.Value, prefix, path string, e environ, errs *[]string) bool {
	t := v.Type()
	set := false
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		key, inline := yamlKey(f)
		if key == "-" || (inline && f.Type.Kind() == reflect.Map) {
			continue // an inline map holds the extension blocks, which no variable sets
		}
		name, p := prefix, path
		if !inline {
			name = prefix + "_" + upperSnake(key)
			p = joinPath(path, key)
		}
		if envField(v.Field(i), name, p, key, e, errs) {
			set = true
		}
	}
	return set
}

// envField sets one field from the variable name, or recurses into a
// struct, a map, or a named list. key is the yaml key of the field.
func envField(fv reflect.Value, name, path, key string, e environ, errs *[]string) bool {
	t := fv.Type()
	switch {
	case t.Kind() == reflect.Struct:
		return envStruct(fv, name, path, e, errs)
	case t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Struct:
		// Fill a copy, and keep it only when a variable set a field, so an
		// absent block stays nil.
		nv := reflect.New(t.Elem())
		if !fv.IsNil() {
			nv.Elem().Set(fv.Elem())
		}
		if envStruct(nv.Elem(), name, path, e, errs) {
			fv.Set(nv)
			return true
		}
		return false
	case t.Kind() == reflect.Map:
		return envMap(fv, name, path, key, e, errs)
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Struct:
		if nameField(t.Elem()) < 0 {
			return false // file-only: the entries have no name to address
		}
		return envNamedList(fv, name, path, e, errs)
	case t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.String:
		return false // file-only
	}
	raw, ok := e.lookup(name)
	if !ok {
		return false
	}
	raw = strings.TrimSpace(raw)
	if err := setScalar(fv, raw); err != nil {
		*errs = append(*errs, fmt.Sprintf("%s=%q: %v", name, raw, err))
		return false
	}
	return true
}

func setScalar(fv reflect.Value, raw string) error {
	t := fv.Type()
	switch {
	case t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Bool:
		// An optional boolean, such as auth.oauth.offline, whose nil means
		// the default.
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("not a boolean")
		}
		fv.Set(reflect.ValueOf(&b))
	case t == durationType:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("not a duration such as 30s or 2h")
		}
		fv.SetInt(int64(d))
	case t.Kind() == reflect.String:
		fv.SetString(raw)
	case t.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("not a boolean")
		}
		fv.SetBool(b)
	case t.Kind() >= reflect.Int && t.Kind() <= reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, t.Bits())
		if err != nil {
			return fmt.Errorf("not an integer")
		}
		fv.SetInt(n)
	case t.Kind() >= reflect.Uint && t.Kind() <= reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, t.Bits())
		if err != nil {
			return fmt.Errorf("not a non-negative integer")
		}
		fv.SetUint(n)
	case t.Kind() == reflect.Float32 || t.Kind() == reflect.Float64:
		n, err := strconv.ParseFloat(raw, t.Bits())
		if err != nil {
			return fmt.Errorf("not a number")
		}
		fv.SetFloat(n)
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.String:
		var items []string
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				items = append(items, s)
			}
		}
		lv := reflect.MakeSlice(t, len(items), len(items))
		for i, s := range items {
			lv.Index(i).SetString(s)
		}
		fv.Set(lv)
	default:
		return fmt.Errorf("type %s cannot be set from a variable", t)
	}
	return nil
}

// Maps and named lists.

// keyRule turns the key segment of a variable into a map key. When loose
// is set, an existing key also matches the segment with case ignored and
// with - equal to _, so FW_AGENTS_CODE_REVIEW_MODEL updates the profile
// code-review from the file.
type keyRule struct {
	convert func(string) string
	loose   bool
}

// keyRules maps the yaml key of a map to its rule. A map that is not in
// the table uses defaultKeyRule: the segment in lowercase, matched loosely.
// That is the rule of every map keyed by a name, such as models, agents,
// providers, and tools.limits.perTool.
var keyRules = map[string]keyRule{
	// env holds variable names, which stay as given: NATS_URL.
	"env": {convert: func(s string) string { return s }},
	// headers holds header names: X_API_KEY is X-Api-Key.
	"headers": {convert: headerKey},
	// options holds the option names of a driver or an adapter, in lower
	// camel case: MAX_TOKENS_FIELD is maxTokensField.
	"options": {convert: lowerCamel},
	// prices holds model selectors: ANTHROPIC_CLAUDE_OPUS_5_5 is
	// anthropic/claude-opus-5-5. The first underscore is the slash, and
	// the others are dashes.
	"prices": {convert: selectorKey},
}

var defaultKeyRule = keyRule{convert: strings.ToLower, loose: true}

func keyRuleFor(yamlKey string) keyRule {
	if r, ok := keyRules[yamlKey]; ok {
		return r
	}
	return defaultKeyRule
}

func headerKey(s string) string {
	return textproto.CanonicalMIMEHeaderKey(strings.ReplaceAll(s, "_", "-"))
}

// lowerCamel turns MAX_TOKENS_FIELD into maxTokensField.
func lowerCamel(s string) string {
	var b strings.Builder
	for i, part := range strings.Split(strings.ToLower(s), "_") {
		if part == "" {
			continue
		}
		if i == 0 {
			b.WriteString(part)
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

// selectorKey turns ANTHROPIC_CLAUDE_OPUS_5_5 into anthropic/claude-opus-5-5.
func selectorKey(s string) string {
	provider, model, ok := strings.Cut(strings.ToLower(s), "_")
	if !ok {
		return provider
	}
	return provider + "/" + strings.ReplaceAll(model, "_", "-")
}

// sameName reports whether two names are equal with case ignored and with
// - equal to _.
func sameName(a, b string) bool {
	return strings.EqualFold(strings.ReplaceAll(a, "-", "_"), strings.ReplaceAll(b, "-", "_"))
}

// mapKeyFor returns the key of the map m for a segment: the converted
// segment, or an existing key that matches it loosely when the rule allows.
func mapKeyFor(m reflect.Value, segment string, rule keyRule) string {
	key := rule.convert(segment)
	if !rule.loose || m.IsNil() || m.MapIndex(reflect.ValueOf(key)).IsValid() {
		return key
	}
	var keys []string
	for _, k := range m.MapKeys() {
		if sameName(k.String(), segment) {
			keys = append(keys, k.String())
		}
	}
	if len(keys) == 0 {
		return key
	}
	sort.Strings(keys)
	return keys[0]
}

// route is one field of an entry struct that a variable can set: its upper
// snake suffix, the field indices from the entry to it, and, for a map of
// strings, the yaml key of the map, whose key follows the suffix.
type route struct {
	suffix string
	index  []int
	mapKey string // the yaml key of a map leaf, or ""
}

// pattern returns the suffix as EnvNames lists it.
func (r route) pattern() string {
	if r.mapKey != "" {
		return r.suffix + "_" + namePlaceholder
	}
	return r.suffix
}

// routesOf lists the routes of an entry struct, sorted by suffix. The name
// field is not a route: the entry name comes from the variable. A map of
// structs or a list of structs inside an entry stays file-only.
func routesOf(t reflect.Type) []route {
	var out []route
	collectRoutes(t, "", nil, true, &out)
	sort.Slice(out, func(i, j int) bool { return out[i].suffix < out[j].suffix })
	return out
}

func collectRoutes(t reflect.Type, prefix string, index []int, top bool, out *[]route) {
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		key, inline := yamlKey(f)
		if key == "-" || (top && !inline && key == "name") || (inline && f.Type.Kind() == reflect.Map) {
			continue
		}
		idx := append(append([]int(nil), index...), i)
		suffix := prefix
		if !inline {
			suffix = joinSnake(prefix, upperSnake(key))
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch {
		case ft.Kind() == reflect.Struct:
			collectRoutes(ft, suffix, idx, inline && top, out)
		case ft.Kind() == reflect.Map:
			if ft.Key().Kind() == reflect.String && ft.Elem().Kind() == reflect.String {
				*out = append(*out, route{suffix: suffix, index: idx, mapKey: key})
			}
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() != reflect.String:
		default:
			*out = append(*out, route{suffix: suffix, index: idx})
		}
	}
}

func joinSnake(prefix, s string) string {
	if prefix == "" {
		return s
	}
	return prefix + "_" + s
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// nameField returns the index of the field with the yaml key name, or -1.
func nameField(t reflect.Type) int {
	for i := range t.NumField() {
		if key, inline := yamlKey(t.Field(i)); !inline && key == "name" && t.Field(i).IsExported() {
			return i
		}
	}
	return -1
}

// fieldByIndex returns the field of v at the index path, with every nil
// pointer on the way allocated.
func fieldByIndex(v reflect.Value, index []int) reflect.Value {
	for _, i := range index {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(i)
	}
	return v
}

// assignment is one variable resolved to the entry it names, the route of
// the field, and the key when the route is a map.
type assignment struct {
	variable string
	entry    string
	route    route
	key      string
	value    string
}

// splitEntry splits the part of a variable after the prefix of a map into
// the entry name, the route, and the map key, by the longest route that
// the part ends with. The second result is false when no route matches.
func splitEntry(rest string, routes []route) (assignment, bool) {
	var best assignment
	bestLen := 0
	for _, r := range routes {
		if r.mapKey != "" {
			marker := "_" + r.suffix + "_"
			i := strings.Index(rest, marker)
			if i <= 0 || i+len(marker) >= len(rest) {
				continue
			}
			if l := len(rest) - i; l > bestLen {
				best, bestLen = assignment{entry: rest[:i], route: r, key: rest[i+len(marker):]}, l
			}
			continue
		}
		if l := len(r.suffix) + 1; strings.HasSuffix(rest, "_"+r.suffix) && len(rest) > l && l > bestLen {
			best, bestLen = assignment{entry: rest[:len(rest)-l], route: r}, l
		}
	}
	return best, bestLen > 0
}

// entryAssignments resolves every variable under the prefix of a map or a
// named list to an assignment, sorted by variable name. A variable that
// names no field is an error that lists the fields.
func entryAssignments(prefix, path string, routes []route, e environ, errs *[]string) []assignment {
	var out []assignment
	for _, name := range e.under(prefix) {
		rest := name[len(prefix)+1:]
		a, ok := splitEntry(rest, routes)
		if !ok {
			*errs = append(*errs, describeMiss(name, prefix, path, rest, routes))
			continue
		}
		a.variable, a.value = name, strings.TrimSpace(e[name])
		out = append(out, a)
	}
	return out
}

// describeMiss words the error for a variable under the prefix of a map
// that ends with no known field.
func describeMiss(name, prefix, path, rest string, routes []route) string {
	patterns := make([]string, len(routes))
	for i, r := range routes {
		patterns[i] = r.pattern()
	}
	for _, r := range routes {
		if rest == r.suffix || (r.mapKey != "" && strings.HasPrefix(rest, r.suffix+"_")) {
			return fmt.Sprintf("%s: no entry name before %s; the form is %s_%s_%s", name, r.suffix, prefix, namePlaceholder, r.pattern())
		}
	}
	return fmt.Sprintf("%s: no field of %s matches; the form is %s_%s_<FIELD> with FIELD one of %s",
		name, path, prefix, namePlaceholder, strings.Join(patterns, ", "))
}

// setRoute sets the field of the entry ev that the assignment names.
func setRoute(ev reflect.Value, a assignment) error {
	fv := fieldByIndex(ev, a.route.index)
	if a.route.mapKey == "" {
		return setScalar(fv, a.value)
	}
	if fv.IsNil() {
		fv.Set(reflect.MakeMap(fv.Type()))
	}
	fv.SetMapIndex(reflect.ValueOf(keyRuleFor(a.route.mapKey).convert(a.key)), reflect.ValueOf(a.value))
	return nil
}

// envMap sets the entries of the map fv from the variables under the
// prefix and reports whether it set any. A map of strings takes one
// variable per key. A map of structs takes one variable per field of an
// entry, and creates the entry when the map has none.
func envMap(fv reflect.Value, prefix, path, key string, e environ, errs *[]string) bool {
	t := fv.Type()
	if t.Key().Kind() != reflect.String {
		return false
	}
	if _, ok := e[prefix]; ok {
		*errs = append(*errs, fmt.Sprintf("%s sets nothing; the form is %s_%s", prefix, prefix, namePlaceholder))
	}
	rule := keyRuleFor(key)
	set := false
	store := func(mk string, v reflect.Value) {
		if fv.IsNil() {
			fv.Set(reflect.MakeMap(t))
		}
		fv.SetMapIndex(reflect.ValueOf(mk), v)
		set = true
	}
	if t.Elem().Kind() != reflect.Struct {
		for _, name := range e.under(prefix) {
			segment := name[len(prefix)+1:]
			if segment == "" {
				*errs = append(*errs, fmt.Sprintf("%s has no key after %s_; the form is %s_%s", name, prefix, prefix, namePlaceholder))
				continue
			}
			v := reflect.New(t.Elem()).Elem()
			if err := setScalar(v, strings.TrimSpace(e[name])); err != nil {
				*errs = append(*errs, fmt.Sprintf("%s=%q: %v", name, e[name], err))
				continue
			}
			store(mapKeyFor(fv, segment, rule), v)
		}
		return set
	}
	routes := routesOf(t.Elem())
	for _, a := range entryAssignments(prefix, path, routes, e, errs) {
		mk := mapKeyFor(fv, a.entry, rule)
		ev := reflect.New(t.Elem()).Elem()
		if !fv.IsNil() {
			if cur := fv.MapIndex(reflect.ValueOf(mk)); cur.IsValid() {
				ev.Set(cur)
			}
		}
		if err := setRoute(ev, a); err != nil {
			*errs = append(*errs, fmt.Sprintf("%s=%q: %v", a.variable, a.value, err))
			continue
		}
		store(mk, ev)
	}
	return set
}

// envNamedList sets the entries of the list fv, whose elements have a name
// field, from the variables under the prefix. An entry matches a variable
// when its name equals the entry segment with case ignored and with -
// equal to _. A new entry is appended with its name in lowercase.
func envNamedList(fv reflect.Value, prefix, path string, e environ, errs *[]string) bool {
	t := fv.Type()
	if _, ok := e[prefix]; ok {
		*errs = append(*errs, fmt.Sprintf("%s sets nothing; the form is %s_%s_<FIELD>", prefix, prefix, namePlaceholder))
	}
	nameIdx := nameField(t.Elem())
	set := false
	for _, a := range entryAssignments(prefix, path, routesOf(t.Elem()), e, errs) {
		idx := -1
		for i := range fv.Len() {
			if sameName(fv.Index(i).Field(nameIdx).String(), a.entry) {
				idx = i
				break
			}
		}
		if idx < 0 {
			fv.Set(reflect.Append(fv, reflect.Zero(t.Elem())))
			idx = fv.Len() - 1
			fv.Index(idx).Field(nameIdx).SetString(strings.ToLower(a.entry))
		}
		if err := setRoute(fv.Index(idx), a); err != nil {
			*errs = append(*errs, fmt.Sprintf("%s=%q: %v", a.variable, a.value, err))
			continue
		}
		set = true
	}
	return set
}

// yamlKey returns the yaml key of a field and whether the field is inline.
func yamlKey(f reflect.StructField) (key string, inline bool) {
	tag := f.Tag.Get("yaml")
	name, opts, _ := strings.Cut(tag, ",")
	for _, o := range strings.Split(opts, ",") {
		if o == "inline" {
			return "", true
		}
	}
	if name == "" {
		name = strings.ToLower(f.Name)
	}
	return name, false
}

// upperSnake turns a yaml key into the upper snake case of a variable name:
// searchThreshold is SEARCH_THRESHOLD, maxCostUSD is MAX_COST_USD, and
// taskTTL is TASK_TTL.
func upperSnake(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		if i > 0 && unicode.IsUpper(r) {
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

// EnvNames lists the name of every variable that sets a configuration key,
// sorted. A map or a named list is listed as a pattern with <NAME> in
// place of the key or the entry name, such as FW_MODELS_<NAME> and
// FW_TOOLS_MCP_<NAME>_ENDPOINT. Aliases are not in the list.
func EnvNames() []string {
	var out []string
	envNames(reflect.TypeOf(fw.Config{}), EnvPrefix, &out)
	sort.Strings(out)
	return out
}

func envNames(t reflect.Type, prefix string, out *[]string) {
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		key, inline := yamlKey(f)
		if key == "-" || (inline && f.Type.Kind() == reflect.Map) {
			continue
		}
		name := prefix
		if !inline {
			name = prefix + "_" + upperSnake(key)
		}
		ft := f.Type
		switch {
		case ft == durationType:
			*out = append(*out, name)
		case ft.Kind() == reflect.Struct:
			envNames(ft, name, out)
		case ft.Kind() == reflect.Pointer && ft.Elem().Kind() == reflect.Struct:
			envNames(ft.Elem(), name, out)
		case ft.Kind() == reflect.Map && ft.Key().Kind() == reflect.String:
			entryNames(ft.Elem(), name, out)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
			if nameField(ft.Elem()) >= 0 {
				entryNames(ft.Elem(), name, out)
			}
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() != reflect.String:
		default:
			*out = append(*out, name)
		}
	}
}

// entryNames lists the patterns of a map or a named list whose entries
// have the type t.
func entryNames(t reflect.Type, prefix string, out *[]string) {
	if t.Kind() != reflect.Struct {
		*out = append(*out, prefix+"_"+namePlaceholder)
		return
	}
	for _, r := range routesOf(t) {
		*out = append(*out, prefix+"_"+namePlaceholder+"_"+r.pattern())
	}
}
