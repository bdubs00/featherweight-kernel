package native

import (
	"encoding/json"
	"net/http"
)

const problemContentType = "application/problem+json"

// Problem is an RFC 9457 problem document. The type is "about:blank", so
// the title is the HTTP status phrase and the detail says what went wrong.
// Signin is an extension member of a 401 when browser sign-in is on: the
// path of the route that signs a person in.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	Signin   string `json:"signin,omitempty"`
}

// problem writes a problem document with its status.
func problem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	w.Header().Set("Content-Type", problemContentType)
	w.WriteHeader(status)
	writeProblemBody(w, r, status, detail)
}

// writeProblemBody writes the body of a problem document after the header.
// The instance is the request path without the query, which can carry
// values that do not belong in a response.
func writeProblemBody(w http.ResponseWriter, r *http.Request, status int, detail string) {
	_ = json.NewEncoder(w).Encode(newProblem(r, status, detail))
}

// newProblem builds the problem document of a request.
func newProblem(r *http.Request, status int, detail string) Problem {
	return Problem{
		Type:     "about:blank",
		Title:    http.StatusText(status),
		Status:   status,
		Detail:   detail,
		Instance: r.URL.Path,
	}
}
