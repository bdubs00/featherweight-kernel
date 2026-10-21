package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestStreamMeasuresTheFirstByte checks that FirstByteMS is the time to
// the first event of the stream, not the time of the whole answer.
func TestStreamMeasuresTheFirstByte(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(80 * time.Millisecond) // the wait for the model
		_, _ = io.WriteString(w, "data: "+`{"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond) // the work of the model
		_, _ = io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	m := testModel(t, srv.URL, "", "m", false)
	start := time.Now()
	resp, err := m.Stream(context.Background(), &fw.Request{Messages: []fw.Message{fw.UserText("hi")}}, nil)
	total := time.Since(start).Milliseconds()
	if err != nil {
		t.Fatal(err)
	}
	if resp.FirstByteMS < 70 || resp.FirstByteMS >= total-100 {
		t.Fatalf("FirstByteMS = %d, want about 80 of a call of %d ms", resp.FirstByteMS, total)
	}
	if resp.QueuedMS != 0 {
		t.Fatalf("QueuedMS = %d, want 0: the kernel sets it", resp.QueuedMS)
	}
}
