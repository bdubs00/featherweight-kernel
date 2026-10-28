package config

import (
	"strings"
	"testing"
)

// TestMemoryFront checks that the variables set tools.memory.front, and
// that a front without an address of http or https, or without the name
// of the variable of its token, stops the load with a message that names
// the key.
func TestMemoryFront(t *testing.T) {
	cfg, err := load("", nil, []string{"FW_TOOLS_MEMORY_FRONT_URL=http://fw-front:8080", "FW_TOOLS_MEMORY_FRONT_TOKEN_ENV=FW_API_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if f := cfg.Tools.Memory.Front; f == nil || f.URL != "http://fw-front:8080" || f.TokenEnv != "FW_API_TOKEN" {
		t.Fatalf("front = %+v", f)
	}
	for env, want := range map[string]string{
		"FW_TOOLS_MEMORY_FRONT_URL=http://fw-front:8080":                                     "tools.memory.front.tokenEnv",
		"FW_TOOLS_MEMORY_FRONT_URL=fw-front:8080,FW_TOOLS_MEMORY_FRONT_TOKEN_ENV=FW_API_TOKEN": "tools.memory.front.url",
	} {
		if _, err := load("", nil, strings.Split(env, ",")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", env, err, want)
		}
	}
}
