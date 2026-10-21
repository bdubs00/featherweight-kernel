package native

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestLifecycleProblemStatuses(t *testing.T) {
	for _, tc := range []struct {
		err      error
		status   int
		guidance string
	}{
		{&fw.SaturationError{MaxRuns: 1, MaxQueuedRuns: 3}, 429, "idempotency key"},
		{errors.Join(fw.ErrStorageUnavailable, errors.New("private storage detail")), 503, "restarts"},
	} {
		w := httptest.NewRecorder()
		(&server{}).fail(w, httptest.NewRequest("POST", "/v1/runs", nil), tc.err)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.guidance) || strings.Contains(w.Body.String(), "private storage") {
			t.Fatalf("response = %d %s", w.Code, w.Body.String())
		}
		if tc.status == 429 && w.Header().Get("Retry-After") == "" {
			t.Fatal("missing Retry-After")
		}
	}
}
