package native

import (
	"bufio"
	"encoding/json"
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
