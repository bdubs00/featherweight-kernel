package native

import (
	"net/http"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// messagesOf returns the data of every fw.message.received of a run.
func messagesOf(t *testing.T, a *fw.Agent, id string) []fw.MessageReceivedData {
	t.Helper()
	var out []fw.MessageReceivedData
	for _, e := range collect(t, a, id) {
		var d fw.MessageReceivedData
		if e.Type == fw.EvMessageReceived && e.Decode(&d) == nil && d.Origin != fw.OriginKernel {
			out = append(out, d)
		}
	}
	return out
}

// TestMessageNamesItsSender checks the sender of a follow-up: the message
// records the subject of the caller, a trusted caller names the party that
// it relays the message for, and a person cannot, as for the id of a run.
func TestMessageNamesItsSender(t *testing.T) {
	ctx := testCtx(t)
	callers := tokenMap{
		"alice-token":  {Subject: "alice", Kind: fw.KindUser},
		"member-token": {Subject: fw.KindToken, Kind: fw.KindToken},
	}
	a := newAgent(t, newScript(endTurn("first"), endTurn("second"), endTurn("third")).model(), fw.WithAuthn(callers))
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"hi","wait":true}`, "Authorization", "Bearer alice-token")
	r := decodeRun(t, resp, b, http.StatusOK)

	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"content":"again"}`, "Authorization", "Bearer alice-token")
	decodeRun(t, resp, b, http.StatusAccepted)
	if _, err := a.Wait(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	from := `{"kind":"agent","name":"reviewer","run":"01RUNOFREVIEWER","member":"reviewer"}`
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"content":"rebase please","from":`+from+`}`, "Authorization", "Bearer alice-token")
	if p := decodeProblem(t, resp, b, http.StatusForbidden); p.Detail != "only the static token or a workload can name the sender of a message" {
		t.Fatalf("detail = %q", p.Detail)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"content":"rebase please","from":`+from+`}`, "Authorization", "Bearer member-token")
	decodeRun(t, resp, b, http.StatusAccepted)
	if _, err := a.Wait(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"content":"x","from":{"kind":"bot","name":"x"}}`, "Authorization", "Bearer member-token")
	decodeProblem(t, resp, b, http.StatusBadRequest)

	msgs := messagesOf(t, a, r.ID)
	if len(msgs) != 2 || msgs[0].By != "alice" || msgs[0].From != nil {
		t.Fatalf("messages = %+v", msgs)
	}
	want := fw.MessageFrom{Kind: fw.FromAgent, Name: "reviewer", Run: "01RUNOFREVIEWER", Member: "reviewer"}
	if msgs[1].By != fw.KindToken || msgs[1].From == nil || *msgs[1].From != want {
		t.Fatalf("relayed message = %+v", msgs[1])
	}
}
