package fw

import "testing"

func TestTitleFrom(t *testing.T) {
	cases := map[string]string{
		"":                         "",
		"  \n\n ":                  "",
		"Fix checkout retries":     "Fix checkout retries",
		"Fix checkout retries.":    "Fix checkout retries",
		"## Release notes\nfor v2": "Release notes",
		"- Investigate the intermittent checkout failure. It started Tuesday.":                                             "Investigate the intermittent checkout failure",
		"Investigate the intermittent checkout failure in the payments service, which started on Tuesday after the deploy": "Investigate the intermittent checkout failure in the payments…",
		"Is it up? Check.": "Is it up? Check",
	}
	for in, want := range cases {
		if got := TitleFrom(in); got != want {
			t.Errorf("TitleFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunTitleFromMetadata(t *testing.T) {
	e := MustEvent("r1", EvRunQueued, RunQueuedData{Agent: "reviewer", Input: "Look at the build", Metadata: map[string]string{MetaTitle: " Fix the build "}})
	r := ProjectRun([]Event{e})
	if r.Title != "Fix the build" {
		t.Fatalf("title = %q", r.Title)
	}
	e = MustEvent("r1", EvRunQueued, RunQueuedData{Agent: "reviewer", Input: "Look at the build. Then fix it."})
	if r = ProjectRun([]Event{e}); r.Title != "Look at the build" {
		t.Fatalf("title = %q", r.Title)
	}
}
