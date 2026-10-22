package fw

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// cutStrings cuts each long string value, also inside nested objects and
// arrays, at a rune boundary with a note, and leaves keys, short strings,
// and other values as they are.
func TestCutStrings(t *testing.T) {
	long := strings.Repeat("a", 100)
	runes := strings.Repeat("é", 10) // 20 bytes; byte 9 is inside a rune
	key := strings.Repeat("k", 40)
	in := json.RawMessage(`{"tool":"bash","input":{"command":"` + long + `","args":["short","` + runes + `",12345678901234567890,true,null],"` + key + `":{"deep":"` + long + `"}}}`)
	got := cutStrings(in, 9)
	var v struct {
		Tool  string `json:"tool"`
		Input struct {
			Command string            `json:"command"`
			Args    []json.RawMessage `json:"args"`
			Deep    map[string]string `json:"-"`
		} `json:"input"`
	}
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("cut data %s: %v", got, err)
	}
	if v.Tool != "bash" || v.Input.Command != "aaaaaaaaa [cut 91 of 100 bytes]" {
		t.Fatalf("cut data = %s", got)
	}
	args := []string{`"short"`, `"éééé [cut 12 of 20 bytes]"`, `12345678901234567890`, `true`, `null`}
	for i, want := range args {
		if string(v.Input.Args[i]) != want {
			t.Fatalf("args[%d] = %s, want %s", i, v.Input.Args[i], want)
		}
	}
	if !strings.Contains(string(got), `"`+key+`":{"deep":"aaaaaaaaa [cut 91 of 100 bytes]"}`) {
		t.Fatalf("the nested value or the long key = %s", got)
	}
	if !bytes.Contains(in, []byte(long)) {
		t.Fatal("cutStrings changed its input")
	}
	// A bound of 0 and data without a long string give the data back as
	// it is, without a decode.
	if out := cutStrings(in, 0); &out[0] != &in[0] {
		t.Fatal("a bound of 0 changed the data")
	}
	short := json.RawMessage(`{"a":"<b>&","n":1.50}`)
	if out := cutStrings(short, 9); &out[0] != &short[0] {
		t.Fatal("data without a long string was decoded")
	}
	if out := cutStrings(nil, 9); out != nil {
		t.Fatalf("empty data = %s", out)
	}
	// An escape counts as its bytes in the scan, and the cut measures the
	// decoded value. HTML characters stay as they are.
	escaped := json.RawMessage(`{"s":"<a>\n<b>\n<c>"}`)
	if out := string(cutStrings(escaped, 5)); out != `{"s":"<a>\n< [cut 6 of 11 bytes]"}` {
		t.Fatalf("escaped data = %s", out)
	}
	// Bytes that are not UTF-8 decode longer than their literal, so the
	// scan does not let them pass.
	bad := json.RawMessage("{\"s\":\"\xff\xff\xff\xff\"}")
	if out := string(cutStrings(bad, 6)); out != `{"s":"��`+" [cut 6 of 12 bytes]\"}" {
		t.Fatalf("data that is not UTF-8 = %q", out)
	}
}

// egressRun runs one tool call with a long input and a nested long string
// on an agent whose egress has the bound max, and returns the published
// fw.tool.requested and fw.tool.responded with the ledger of the run.
func egressRun(t *testing.T, max int) (map[string]Event, []Event) {
	t.Helper()
	ctx := testCtx(t)
	bus := NewMemBus()
	cfg := DefaultConfig()
	cfg.Bus.Egress = "fw.events"
	cfg.Bus.EgressMaxBytes = max
	got := make(chan Event, 100)
	stop, err := bus.Subscribe(ctx, "fw.events.>", func(e Event) error {
		got <- e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	input := `{"text":"` + strings.Repeat("x", 5000) + `","nested":{"list":["` + strings.Repeat("y", 300) + `",7]}}`
	a := newTestAgent(t, newScript(callTools(use("c1", "echo", input)), endTurn("done")).Model(), WithConfig(cfg), WithBus(bus), WithTools(echoTool(nil)))
	r, err := a.Run(ctx, "echo a long text")
	mustState(t, r, err, StateCompleted)
	out := map[string]Event{}
	deadline := time.After(5 * time.Second)
	for len(out) < 2 {
		select {
		case e := <-got:
			if e.Type == EvToolRequested || e.Type == EvToolResponded {
				out[e.Type] = e
			}
		case <-deadline:
			t.Fatalf("the egress published %d of the two tool events", len(out))
		}
	}
	return out, ledgerEvents(t, a.Ledger(), r.ID)
}

// The egress publishes each long string of a tool event cut to the bound,
// with the envelope whole, and the ledger keeps the event whole.
func TestEgressCutsLongStrings(t *testing.T) {
	published, ledger := egressRun(t, 256)
	var req ToolRequestedData
	if err := published[EvToolRequested].Decode(&req); err != nil {
		t.Fatal(err)
	}
	var in struct {
		Text   string `json:"text"`
		Nested struct {
			List []json.RawMessage `json:"list"`
		} `json:"nested"`
	}
	if err := json.Unmarshal(req.Input, &in); err != nil {
		t.Fatalf("published input %s: %v", req.Input, err)
	}
	if in.Text != strings.Repeat("x", 256)+" [cut 4744 of 5000 bytes]" || len(in.Nested.List) != 2 ||
		string(in.Nested.List[0]) != `"`+strings.Repeat("y", 256)+` [cut 44 of 300 bytes]"` || string(in.Nested.List[1]) != "7" {
		t.Fatalf("published input = %s", req.Input)
	}
	if req.Tool != "echo" || req.CallID != "c1" {
		t.Fatalf("published request = %+v", req)
	}
	var res ToolRespondedData
	if err := published[EvToolResponded].Decode(&res); err != nil {
		t.Fatal(err)
	}
	if text := TextOf(res.Content); text != strings.Repeat("x", 256)+" [cut 4744 of 5000 bytes]" {
		t.Fatalf("published result = %.300s", text)
	}
	// The envelope of the published event is the envelope of the ledger
	// event, and the ledger holds the whole input and output.
	for _, e := range ledger {
		p, ok := published[e.Type]
		if !ok {
			continue
		}
		if p.ID != e.ID || p.Type != e.Type || !p.Time.Equal(e.Time) || p.Subject != e.Subject || p.Seq != e.Seq || p.Node != e.Node || p.Source != e.Source {
			t.Fatalf("published envelope = %+v, ledger %+v", p, e)
		}
		if !strings.Contains(string(e.Data), strings.Repeat("x", 5000)) {
			t.Fatalf("the ledger event %s was cut: %.200s", e.Type, e.Data)
		}
		if strings.Contains(string(e.Data), "[cut ") {
			t.Fatalf("the ledger event %s holds a note of a cut", e.Type)
		}
	}
}

// With the bound 0, the default, the egress publishes the data whole.
func TestEgressWithoutBoundPublishesWhole(t *testing.T) {
	published, ledger := egressRun(t, 0)
	for _, e := range ledger {
		if p, ok := published[e.Type]; ok && !bytes.Equal(p.Data, e.Data) {
			t.Fatalf("published %s = %.200s, ledger %.200s", e.Type, p.Data, e.Data)
		}
	}
	if !strings.Contains(string(published[EvToolRequested].Data), strings.Repeat("x", 5000)) {
		t.Fatal("the published input was cut without a bound")
	}
}
