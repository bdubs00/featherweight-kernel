package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

// The loader names the profiles that it read from files, so the kernel
// knows which profiles the configuration defines.
func TestLoadNamesFileProfiles(t *testing.T) {
	cleanEnv(t)
	home := os.Getenv("FW_HOME")
	writeFile(t, filepath.Join(home, "agents", "free.yaml"), "system: from the file\n")
	path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), "agents:\n  fixed: {system: from fw.yaml}\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.FileProfiles["free"] || cfg.FileProfiles["fixed"] || len(cfg.Agents) != 2 {
		t.Fatalf("FileProfiles = %v, agents %v", cfg.FileProfiles, cfg.Agents)
	}
}

// The store writes one file per profile, the loader reads it back, and a
// delete removes it.
func TestProfileStoreRoundTrip(t *testing.T) {
	cleanEnv(t)
	home := os.Getenv("FW_HOME")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenProfileStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := fw.AgentProfile{
		Name:         "writer",
		Description:  "Writes prose",
		System:       "You write.\nCost: $5, ${NOT_A_VAR}, $$literal, $$${A}.",
		Instructions: []string{"Be brief.", "prompts/missing.md"},
		Tools:        []string{"files", "git.*"},
		Skills:       []string{},
		Model:        "anthropic/claude-haiku-5-5",
		Effort:       "low",
		Budget:       &fw.Budget{MaxTurns: 3},
		Access:       &fw.Access{Owners: []string{"local:bdubs"}, Viewers: []string{"*"}},
	}
	if err := store.Put(ctx, p); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "agents", "writer.yaml")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %o, want 0600", st.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("the file is not YAML: %v\n%s", err, data)
	}
	if raw["name"] != "writer" || raw["model"] != "anthropic/claude-haiku-5-5" || raw["effort"] != "low" {
		t.Errorf("file:\n%s", data)
	}
	// The texts are escaped for the loader: ${ is $${ and $$ is $$$$.
	if sys, _ := raw["system"].(string); !strings.Contains(sys, "$5, $${NOT_A_VAR}, $$$$literal, $$$$$${A}.") {
		t.Errorf("system in the file: %q", sys)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "agents")); len(entries) != 1 {
		t.Errorf("the temporary file was left behind: %v", entries)
	}

	// A reload sees the profile as it was put.
	cfg2, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := cfg2.Agents["writer"]
	if !ok {
		t.Fatalf("the reload lacks the profile: %v", sortedKeys(cfg2.Agents))
	}
	if got.System != p.System || !slices.Equal(got.Instructions, p.Instructions) || !slices.Equal(got.Tools, p.Tools) ||
		got.Skills == nil || len(got.Skills) != 0 || got.Model != p.Model || got.Budget == nil || got.Budget.MaxTurns != 3 ||
		got.Access == nil || !slices.Equal(got.Access.Owners, []string{"local:bdubs"}) || got.Description != p.Description {
		t.Errorf("reloaded profile: %+v (budget %+v, access %+v)", got, got.Budget, got.Access)
	}

	// A second put replaces the file, and a delete removes it.
	p.Description = "Edits prose"
	if err := store.Put(ctx, p); err != nil {
		t.Fatal(err)
	}
	cfg3, _ := Load("")
	if cfg3.Agents["writer"].Description != "Edits prose" {
		t.Errorf("the second put did not replace the file: %+v", cfg3.Agents["writer"])
	}
	if err := store.Delete(ctx, "writer"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file remains: %v", err)
	}
	if err := store.Delete(ctx, "writer"); err != nil {
		t.Errorf("a second delete is an error: %v", err)
	}
	cfg4, _ := Load("")
	if _, ok := cfg4.Agents["writer"]; ok {
		t.Error("the reload still has the deleted profile")
	}
}

// The store writes any profile, and the loader decides: fw.yaml wins over
// a file of the same name at the next load.
func TestProfileStoreAndLoaderOrder(t *testing.T) {
	cleanEnv(t)
	home := os.Getenv("FW_HOME")
	writeFile(t, filepath.Join(home, "agents", "shadowed.yaml"), "system: from the file\n")
	writeFile(t, filepath.Join(home, "agents", "free.yaml"), "system: from the file too\n")
	path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), "agents:\n  fixed: {system: from fw.yaml}\n  shadowed: {system: fw.yaml wins}\n")
	t.Setenv("FW_AGENTS_FROMENV_MODEL", "anthropic/claude-haiku-5-5")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenProfileStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if cfg.FileProfiles["shadowed"] || cfg.FileProfiles["fixed"] || cfg.FileProfiles["fromenv"] || !cfg.FileProfiles["free"] {
		t.Errorf("FileProfiles = %v", cfg.FileProfiles)
	}
	// A file for a profile that fw.yaml defines is written but lost at the
	// next load. The kernel refuses such a change before it reaches the store.
	if err := store.Put(ctx, fw.AgentProfile{Name: "shadowed", System: "changed"}); err != nil {
		t.Errorf("put shadowed: %v", err)
	}
	if err := store.Put(ctx, fw.AgentProfile{Name: "free", System: "changed"}); err != nil {
		t.Errorf("put of a file profile: %v", err)
	}
	if err := store.Put(ctx, fw.AgentProfile{Name: "new", System: "new"}); err != nil {
		t.Errorf("put of a new profile: %v", err)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Agents["free"].System != "changed" || cfg2.Agents["new"].System != "new" || cfg2.Agents["shadowed"].System != "fw.yaml wins" {
		t.Errorf("reload: free %q new %q shadowed %q", cfg2.Agents["free"].System, cfg2.Agents["new"].System, cfg2.Agents["shadowed"].System)
	}
}

// The store writes to the file that declares the name, so a hand-named
// file keeps its place, and it refuses a profile that the next load would
// refuse.
func TestProfileStoreFilesAndValidation(t *testing.T) {
	cleanEnv(t)
	home := os.Getenv("FW_HOME")
	writeFile(t, filepath.Join(home, "agents", "any-file.yml"), "name: named\nsystem: old\n")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenProfileStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, fw.AgentProfile{Name: "named", System: "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "agents", "named.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a second file was written for a name that a file declares")
	}
	cfg2, _ := Load("")
	if cfg2.Agents["named"].System != "new" {
		t.Errorf("reload: %+v", cfg2.Agents["named"])
	}
	if err := store.Delete(ctx, "named"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "agents", "any-file.yml")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the declaring file remains after the delete")
	}

	for _, tc := range []struct {
		p    fw.AgentProfile
		want string
	}{
		{fw.AgentProfile{Name: "x", Model: "nowhere"}, "agents.x.model"},
		{fw.AgentProfile{Name: "x", Effort: "huge"}, "agents.x.effort"},
		{fw.AgentProfile{Name: "x", Access: &fw.Access{Owners: []string{""}}}, "agents.x.access.owners[0] is empty"},
		{fw.AgentProfile{Name: "x", Access: &fw.Access{Viewers: []string{"kind:robot"}}}, "names no kind"},
		{fw.AgentProfile{Name: "x", Budget: &fw.Budget{MaxTurns: -1}}, "agents.x.budget.maxTurns -1"},
		{fw.AgentProfile{}, "needs a name"},
	} {
		err := store.Put(ctx, tc.p)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("put %+v: %v, want %q", tc.p, err, tc.want)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "agents")); len(entries) != 0 {
		t.Errorf("a refused profile left a file: %v", entries)
	}
}

func TestEscapeDollars(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	for _, s := range []string{"plain", "$5", "${A}", "$${A}", "$$", "$$$", "$$${A}", "${", "$", "a$", "$$$${", "${A:-${B}}"} {
		esc := escapeDollars(s)
		got, err := expand(esc, lookup)
		if err != nil || got != s {
			t.Errorf("expand(escape(%q) = %q) = %q, %v", s, esc, got, err)
		}
	}
}

func TestLoadProfile(t *testing.T) {
	cleanEnv(t)
	t.Setenv("PROFILE_MODEL", "anthropic/claude-haiku-5-5")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "prompt.md"), "from a file next to the profile")
	path := writeFile(t, filepath.Join(dir, "helper.yaml"), "description: Helps\nmodel: ${PROFILE_MODEL}\nsystem: prompt.md\ninstructions: [\"Be kind.\", prompt.md]\naccess: {owners: [alice]}\n")
	p, err := LoadProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "helper" || p.Model != "anthropic/claude-haiku-5-5" || p.System != "from a file next to the profile" ||
		!slices.Equal(p.Instructions, []string{"Be kind.", "from a file next to the profile"}) || p.Access == nil || p.Access.Owners[0] != "alice" {
		t.Errorf("profile: %+v", p)
	}
	named := writeFile(t, filepath.Join(dir, "other.yaml"), "name: named\nsystem: You help.\n")
	if p, err := LoadProfile(named); err != nil || p.Name != "named" || p.System != "You help." {
		t.Errorf("named profile: %+v, %v", p, err)
	}
	bad := writeFile(t, filepath.Join(dir, "bad.yaml"), "sytem: typo\n")
	if _, err := LoadProfile(bad); err == nil || !strings.Contains(err.Error(), "sytem") {
		t.Errorf("unknown key: %v", err)
	}
	if _, err := LoadProfile(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("a missing file loads")
	}
}
