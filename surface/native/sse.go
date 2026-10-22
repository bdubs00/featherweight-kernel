package native

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// stream writes the events of a run from a sequence number as server-sent
// events until the run ends or the client goes away. Each event has the
// fields id (the sequence number), event (the type), and data (the event as
// JSON). A comment line keeps an idle stream open.
func (s *server) stream(w http.ResponseWriter, r *http.Request, id string, from uint64) {
	ctx := r.Context()
	events, err := s.a.Events(ctx, id, from)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	// A server with a write timeout would cut a long stream.
	_ = rc.SetWriteDeadline(time.Time{})
	bw := bufio.NewWriter(w)
	flush := func() bool {
		if bw.Flush() != nil {
			return false
		}
		_ = rc.Flush()
		return true
	}
	if !flush() {
		return
	}
	tick := time.NewTicker(s.keepAlive)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := bw.WriteString(": keep-alive\n\n"); err != nil || !flush() {
				return
			}
		case e, ok := <-events:
			if !ok {
				return
			}
			if writeEvent(bw, e) != nil || !flush() {
				return
			}
			tick.Reset(s.keepAlive)
		}
	}
}

// writeEvent writes one event. JSON from encoding/json holds no newline, so
// the data fits on one line.
func writeEvent(bw *bufio.Writer, e fw.Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	bw.WriteString("id: ")
	bw.WriteString(strconv.FormatUint(e.Seq, 10))
	bw.WriteString("\nevent: ")
	bw.WriteString(e.Type)
	bw.WriteString("\ndata: ")
	bw.Write(data)
	_, err = bw.WriteString("\n\n")
	return err
}

// live streams the text of a run as the model produces it, as server-sent
// events (fw.Agent.Live): delta, tool, done, and idle, each with its data
// as JSON and no id, because nothing of it is in the ledger and a client
// that connects again gets the pieces from then on. A comment line keeps
// the stream open. The stream ends when the run finishes, when the client
// goes, or when the client falls behind and the run drops it.
func (s *server) live(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	events, err := s.a.Live(ctx, r.PathValue("id"))
	if errors.Is(err, fw.ErrLiveFull) {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		problem(w, r, http.StatusTooManyRequests, fmt.Sprintf("the run has %d live clients, which is the most it takes", fw.MaxLiveClients))
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	bw := bufio.NewWriter(w)
	flush := func() bool {
		if bw.Flush() != nil {
			return false
		}
		_ = rc.Flush()
		return true
	}
	if !flush() {
		return
	}
	tick := time.NewTicker(s.keepAlive)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := bw.WriteString(": keep-alive\n\n"); err != nil || !flush() {
				return
			}
		case e, ok := <-events:
			if !ok {
				return
			}
			data, err := json.Marshal(e)
			if err != nil {
				return
			}
			bw.WriteString("event: ")
			bw.WriteString(e.Type)
			bw.WriteString("\ndata: ")
			bw.Write(data)
			if _, err := bw.WriteString("\n\n"); err != nil || !flush() {
				return
			}
			tick.Reset(s.keepAlive)
		}
	}
}
