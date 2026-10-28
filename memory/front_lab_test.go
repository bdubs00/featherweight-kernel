package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// A caller with the static token or a workload is no person, so its change
// counts as one of the agent.
func TestByOfAProcess(t *testing.T) {
	for _, kind := range []string{fw.KindToken, fw.KindWorkload} {
		ctx := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "token", Kind: kind})
		if by, _ := byOf(ctx); by != "agent" {
			t.Errorf("%s: by = %q", kind, by)
		}
	}
	ctx := fw.WithPrincipal(context.Background(), &fw.Principal{Subject: "ada@example.org", Kind: fw.KindUser})
	if by, _ := byOf(ctx); by != "person:ada@example.org" {
		t.Errorf("person: by = %q", by)
	}
}

// The error of a front that refuses the connection names no address.
func TestFrontErrorHidesTheAddress(t *testing.T) {
	f, err := newFront(&fw.MemoryFrontConfig{URL: "http://127.0.0.1:1", TokenEnv: "FW_TEST_NO_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.do(context.Background(), "GET", "/v1/knowhow/notes", nil, nil, nil)
	if err == nil || strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), ":1") {
		t.Fatalf("err = %v", err)
	}
}

func TestShownKB(t *testing.T) {
	for size, want := range map[int]float64{0: 0, 1: 0.1, 40: 0.1, 103: 0.2, 1024: 1, 3174: 3.1} {
		if got := shownKB(size); got != want {
			t.Errorf("shownKB(%d) = %v, want %v", size, got, want)
		}
	}
}
