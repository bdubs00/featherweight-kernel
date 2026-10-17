package native

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// writerTool writes each entry of its input, a map of path to text,
// through the workspace of the call.
var writerTool = fw.NewTool(fw.ToolSpec{Name: "writer"}, func(ctx context.Context, in json.RawMessage) (fw.ToolResult, error) {
	var files map[string]string
	_ = json.Unmarshal(in, &files)
	for name, text := range files {
		if err := fw.FilesFrom(ctx).WriteFile(name, []byte(text)); err != nil {
			return fw.ErrorResult(err.Error()), nil
		}
	}
	return fw.TextResult("written"), nil
})

func TestFileRoutes(t *testing.T) {
	ctx := testCtx(t)
	ws, err := fw.DirFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newScript(callTool("c1", "writer", `{"docs/a.md":"# A\n","page.html":"<script>alert(1)</script>"}`), endTurn("done"),
		callTool("c2", "writer", `{"x.md":"x"}`), endTurn("done"))
	a := manageAgent(t, s.model(), fw.WithFiles(ws), fw.WithTools(writerTool))
	srv := serve(t, a)
	actx := fw.WithPrincipal(ctx, people["alice-token"])
	r, err := a.Run(actx, "write docs", fw.AsAgent("reviewer"))
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	base := srv.URL + "/v1/runs/" + r.ID + "/files"

	// bob views reviewer, so he reads its files.
	var list struct{ Files []fw.RunFile }
	resp, b := do(t, "GET", base, "", "Authorization", asBob)
	decodeJSON(t, resp, b, http.StatusOK, &list)
	if len(list.Files) != 2 || list.Files[0].Path == "" {
		t.Fatalf("list = %s", b)
	}
	var v view.FileView
	resp, b = do(t, "GET", base+"/docs/a.md", "", "Authorization", asBob, "Accept", "application/json")
	decodeJSON(t, resp, b, http.StatusOK, &v)
	if v.Path != "docs/a.md" || v.Text != "# A\n" || v.Op != fw.FileCreated {
		t.Fatalf("view = %s", b)
	}
	// The raw content never runs on this origin.
	resp, b = do(t, "GET", base+"/page.html", "", "Authorization", asBob)
	if resp.StatusCode != http.StatusOK || string(b) != "<script>alert(1)</script>" {
		t.Fatalf("raw = %d %s", resp.StatusCode, b)
	}
	for k, want := range map[string]string{
		"Content-Security-Policy": "sandbox",
		"X-Content-Type-Options":  "nosniff",
		"Content-Disposition":     "attachment; filename=page.html",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// A path that the run did not change answers 404, also through "..".
	for _, p := range []string{"/other.md", "/docs/../other.md"} {
		resp, b = do(t, "GET", base+p, "", "Authorization", asAlice)
		decodeProblem(t, resp, b, http.StatusNotFound)
	}
	// The read authorizes as the run does.
	shared, err := a.Run(actx, "shared work", fw.AsAgent("shared"))
	if err != nil {
		t.Fatal(err)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/runs/"+shared.ID+"/files/x.md", "", "Authorization", asBob, "Accept", "application/json")
	decodeProblem(t, resp, b, http.StatusForbidden)
	resp, _ = do(t, "GET", base, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no credential: status %d", resp.StatusCode)
	}
}
