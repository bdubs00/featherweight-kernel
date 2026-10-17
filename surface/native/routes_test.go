package native

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// answers is a model that gives the texts in order and then the last text
// on every call.
func answers(texts ...string) fw.Model {
	var n atomic.Int32
	return fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		i := min(int(n.Add(1))-1, len(texts)-1)
		return endTurn(texts[i]), nil
	}}
}

// builderAccess lets alice operate the default agent, which runs the
// builder conversation. The configuration of manageAgent is closed.
var builderAccess = fw.WithProfile(fw.AgentProfile{Name: "default", Access: &fw.Access{Operators: []string{"alice"}}})

// keys returns the keys of a JSON object.
func keys(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode: %v: %s", err, b)
	}
	return m
}

func TestCreateTitle(t *testing.T) {
	a := newAgent(t, newScript(endTurn("a"), endTurn("b"), endTurn("c")).model())
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"do the thing","title":"  Fix checkout retries  ","metadata":{"team":"pay","fw.title":"old"},"wait":true}`)
	r := decodeRun(t, resp, b, http.StatusOK)
	if r.Title != "Fix checkout retries" || r.Metadata[fw.MetaTitle] != "Fix checkout retries" || r.Metadata["team"] != "pay" {
		t.Fatalf("run = %s", b)
	}
	// Without title, the projection makes one from the input.
	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"Look at the logs. Then more.","wait":true}`)
	if r := decodeRun(t, resp, b, http.StatusOK); r.Title != "Look at the logs" || r.Metadata[fw.MetaTitle] != "" {
		t.Fatalf("run = %s", b)
	}
	// 200 characters pass, also when each takes two bytes, and 201 do not.
	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"x","title":"`+strings.Repeat("é", MaxTitleRunes)+`","wait":true}`)
	if r := decodeRun(t, resp, b, http.StatusOK); r.Title != strings.Repeat("é", MaxTitleRunes) {
		t.Fatalf("title = %q", r.Title)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"x","title":"`+strings.Repeat("é", MaxTitleRunes+1)+`"}`)
	if p := decodeProblem(t, resp, b, http.StatusBadRequest); !strings.Contains(p.Detail, "title") {
		t.Errorf("detail = %q", p.Detail)
	}
}

func TestToolsCatalog(t *testing.T) {
	var calls atomic.Int32
	a := newAgent(t, newScript().model(), fw.WithTools(rmTool(&calls)))
	srv := serve(t, a)
	resp, b := do(t, "GET", srv.URL+"/v1/tools", "")
	var body struct {
		Tools []map[string]any `json:"tools"`
	}
	decodeJSON(t, resp, b, http.StatusOK, &body)
	if len(body.Tools) != 1 || body.Tools[0]["name"] != "rm" || body.Tools[0]["destructive"] != true {
		t.Fatalf("tools = %s", b)
	}
	if text, _ := body.Tools[0]["text"].(string); text == "" {
		t.Fatalf("the tool has no text: %s", b)
	}
	resp, b = do(t, "GET", serve(t, newAgent(t, newScript().model())).URL+"/v1/tools", "")
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(b)) != `{"tools":[]}` {
		t.Fatalf("no tools = %d %s", resp.StatusCode, b)
	}
}

func TestFileDownload(t *testing.T) {
	ctx := testCtx(t)
	ws, err := fw.DirFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newScript(callTool("c1", "writer", `{"page.html":"<b>x</b>","naïve résumé.txt":"cv","100%.txt":"full"}`), endTurn("done"))
	a := manageAgent(t, s.model(), fw.WithFiles(ws), fw.WithTools(writerTool))
	srv := serve(t, a)
	r, err := a.Run(fw.WithPrincipal(ctx, people["alice-token"]), "write", fw.AsAgent("reviewer"))
	if err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	base := srv.URL + "/v1/runs/" + r.ID + "/files/"
	for name, want := range map[string]string{
		"page.html":        `attachment; filename=page.html`,
		"naïve résumé.txt": `attachment; filename="na_ve r_sum_.txt"; filename*=UTF-8''na%C3%AFve%20r%C3%A9sum%C3%A9.txt`,
		"100%.txt":         `attachment; filename=100_.txt; filename*=UTF-8''100%25.txt`,
	} {
		// download=1 answers the raw bytes, also to a client that accepts
		// JSON, as the page does when it saves a file.
		resp, b := do(t, "GET", base+url.PathEscape(name)+"?download=1", "", "Authorization", asBob, "Accept", "application/json")
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Disposition") != want ||
			resp.Header.Get("Content-Security-Policy") != "sandbox" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %d %q %v", name, resp.StatusCode, resp.Header.Get("Content-Disposition"), resp.Header)
		}
		if strings.HasPrefix(string(b), "{") {
			t.Errorf("%s: the body is JSON: %s", name, b)
		}
	}
	// Without download, a JSON client gets the view.
	resp, b := do(t, "GET", base+"page.html", "", "Authorization", asBob, "Accept", "application/json")
	var v view.FileView
	decodeJSON(t, resp, b, http.StatusOK, &v)
	resp, b = do(t, "GET", base+"page.html?download=yes", "", "Authorization", asBob)
	decodeProblem(t, resp, b, http.StatusBadRequest)
	resp, b = do(t, "GET", base+"page.html?download=1", "")
	decodeProblem(t, resp, b, http.StatusUnauthorized)
}

func TestContentDisposition(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":   `attachment; filename=report.pdf`,
		"my file.txt":  `attachment; filename="my file.txt"`,
		`a"b.txt`:      `attachment; filename=a_b.txt; filename*=UTF-8''a%22b.txt`,
		"x\ny\x7f.txt": `attachment; filename=xy.txt`,
		`dir\x.txt`:    `attachment; filename=dir_x.txt`,
		"日本.md":        `attachment; filename=__.md; filename*=UTF-8''%E6%97%A5%E6%9C%AC.md`,
		"":             `attachment; filename=file`,
		" \t ":         `attachment; filename=file`,
		"..":           `attachment; filename=file`,
		// Format characters leave both forms of the name: a right-to-left
		// override, a zero-width space, an isolate, and a byte order mark.
		"invoice\u202etxt.exe": `attachment; filename=invoicetxt.exe`,
		"\ufeffa\u200b.txt":    `attachment; filename=a.txt`,
		"日本\u2066\u202e.md":    `attachment; filename=__.md; filename*=UTF-8''%E6%97%A5%E6%9C%AC.md`,
		"\u202e\u2066\u200b":   `attachment; filename=file`,
	} {
		if got := contentDisposition(in); got != want {
			t.Errorf("contentDisposition(%q) = %q, want %q", in, got, want)
		}
	}
}
