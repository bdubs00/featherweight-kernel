package native

import (
	"net/http"
	"strconv"
	"strings"
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

// TestReactions checks POST /v1/runs/{id}/reactions: a person reacts as
// themself, a thumb on the end of the completed run records a quality
// receipt, a trusted caller names the person, and the answer is the run
// object with the counts.
func TestReactions(t *testing.T) {
	callers := tokenMap{
		"alice-token":  {Subject: "alice", Kind: fw.KindUser},
		"member-token": {Subject: fw.KindToken, Kind: fw.KindToken},
	}
	a := newAgent(t, newScript(endTurn("done")).model(), fw.WithAuthn(callers))
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"hi","wait":true}`, "Authorization", "Bearer alice-token")
	r := decodeRun(t, resp, b, http.StatusOK)
	fin := r.Seq

	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/reactions", `{"seq":`+itoa(fin)+`,"reaction":"up","note":"Exactly right."}`, "Authorization", "Bearer alice-token")
	got := decodeRun(t, resp, b, http.StatusOK)
	if got.State != fw.StateCompleted || got.Reactions[fw.ReactionUp] != 1 {
		t.Fatalf("run = %s", b)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/reactions", `{"reaction":"heart","by":"bob"}`, "Authorization", "Bearer alice-token")
	if p := decodeProblem(t, resp, b, http.StatusForbidden); p.Detail != "only the static token or a workload can name the person of a reaction" {
		t.Fatalf("detail = %q", p.Detail)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/reactions", `{"reaction":"heart","by":"bob"}`, "Authorization", "Bearer member-token")
	if got := decodeRun(t, resp, b, http.StatusOK); got.Reactions[fw.ReactionHeart] != 1 {
		t.Fatalf("run = %s", b)
	}
	for _, body := range []string{`{"reaction":"fire"}`, `{"seq":9999,"reaction":"up"}`, `{"reaction":"up","extra":1}`} {
		resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/reactions", body, "Authorization", "Bearer alice-token")
		decodeProblem(t, resp, b, http.StatusBadRequest)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs/nope/reactions", `{"reaction":"up"}`, "Authorization", "Bearer alice-token")
	decodeProblem(t, resp, b, http.StatusNotFound)

	var reactions []fw.ReactionData
	var receipts int
	for _, e := range collect(t, a, r.ID) {
		switch e.Type {
		case fw.EvRunReacted:
			var d fw.ReactionData
			_ = e.Decode(&d)
			reactions = append(reactions, d)
		case fw.EvRunAnnotated:
			var d fw.QualityAnnotatedData
			if e.Decode(&d) == nil && d.Quality != nil && d.Quality.Verification == "accepted" {
				receipts++
			}
		}
	}
	want := []fw.ReactionData{
		{Seq: fin, Reaction: fw.ReactionUp, By: "alice", Note: "Exactly right.", Receipt: true},
		{Reaction: fw.ReactionHeart, By: "bob"},
	}
	if len(reactions) != 2 || reactions[0] != want[0] || reactions[1] != want[1] || receipts != 1 {
		t.Fatalf("reactions = %+v, %d receipts", reactions, receipts)
	}
}

func itoa(n uint64) string { return strconv.FormatUint(n, 10) }

// TestInlineProfileCompaction checks the block compaction of the inline
// profile of POST /v1/runs: a good block reaches the resolved profile of
// the run, and a bad one is a 400 that names the key.
func TestInlineProfileCompaction(t *testing.T) {
	a := newAgent(t, newScript(endTurn("done")).model())
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"hi","wait":true,"profile":{"compaction":{"at":4096}}}`)
	r := decodeRun(t, resp, b, http.StatusOK)
	var q fw.RunQueuedData
	for _, e := range collect(t, a, r.ID) {
		if e.Type == fw.EvRunQueued {
			_ = e.Decode(&q)
		}
	}
	if q.Resolved == nil || q.Resolved.Compaction == nil || q.Resolved.Compaction.At != 4096 {
		t.Fatalf("resolved = %+v", q.Resolved)
	}
	resp, b = do(t, "POST", srv.URL+"/v1/runs", `{"input":"hi","profile":{"compaction":{"at":-1}}}`)
	if p := decodeProblem(t, resp, b, http.StatusBadRequest); !strings.Contains(p.Detail, "profile.compaction.at") {
		t.Fatalf("detail = %q", p.Detail)
	}
}

// TestPinnedMessage checks pinned in the body of a follow-up: any caller
// who can send pins a message, and the bound of the pinned messages is a
// 400 that names it.
func TestPinnedMessage(t *testing.T) {
	ctx := testCtx(t)
	callers := tokenMap{"alice-token": {Subject: "alice", Kind: fw.KindUser}}
	a := newAgent(t, newScript(endTurn("first"), endTurn("second")).model(), fw.WithAuthn(callers))
	srv := serve(t, a)
	resp, b := do(t, "POST", srv.URL+"/v1/runs", `{"input":"hi","wait":true}`, "Authorization", "Bearer alice-token")
	r := decodeRun(t, resp, b, http.StatusOK)
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"content":"Always answer in French.","pinned":true}`, "Authorization", "Bearer alice-token")
	decodeRun(t, resp, b, http.StatusAccepted)
	if _, err := a.Wait(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	msgs := messagesOf(t, a, r.ID)
	if len(msgs) != 1 || !msgs[0].Pinned || msgs[0].By != "alice" {
		t.Fatalf("messages = %+v", msgs)
	}
	big := strings.Repeat("x", fw.MaxPinnedBytes)
	resp, b = do(t, "POST", srv.URL+"/v1/runs/"+r.ID+"/messages", `{"content":"`+big+`","pinned":true}`, "Authorization", "Bearer alice-token")
	if p := decodeProblem(t, resp, b, http.StatusBadRequest); !strings.Contains(p.Detail, "64 KB") {
		t.Fatalf("detail = %q", p.Detail)
	}
}
