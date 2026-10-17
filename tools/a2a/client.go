package a2a

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"mime"
	"net/http"
	"strings"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// JSON-RPC methods of A2A protocol 1.0.
const (
	MethodSendMessage          = "SendMessage"
	MethodSendStreamingMessage = "SendStreamingMessage"
	MethodGetTask              = "GetTask"
	MethodListTasks            = "ListTasks"
	MethodCancelTask           = "CancelTask"
	MethodSubscribeToTask      = "SubscribeToTask"
)

const maxSSELine = 10 << 20 // bytes in one server-sent event line

// Client calls an A2A agent over the JSON-RPC binding of protocol 1.0. It
// uses the core types of github.com/a2aproject/a2a-go/v2/a2a on the wire.
//
// fw does not use the SDK client package a2aclient, because that package
// imports golang.org/x/mod, which go.sum does not list.
type Client struct {
	URL  string       // the JSON-RPC endpoint from the agent card
	HTTP *http.Client // nil means http.DefaultClient
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// errorCodes maps the A2A JSON-RPC error codes to the SDK errors, so that
// errors.Is works on the caller side.
var errorCodes = map[int]error{
	-32700: sdk.ErrParseError,
	-32600: sdk.ErrInvalidRequest,
	-32601: sdk.ErrMethodNotFound,
	-32602: sdk.ErrInvalidParams,
	-32603: sdk.ErrInternalError,
	-32000: sdk.ErrServerError,
	-32001: sdk.ErrTaskNotFound,
	-32002: sdk.ErrTaskNotCancelable,
	-32003: sdk.ErrPushNotificationNotSupported,
	-32004: sdk.ErrUnsupportedOperation,
	-32005: sdk.ErrUnsupportedContentType,
	-32006: sdk.ErrInvalidAgentResponse,
	-32007: sdk.ErrExtendedCardNotConfigured,
	-32008: sdk.ErrExtensionSupportRequired,
	-32009: sdk.ErrVersionNotSupported,
	-31401: sdk.ErrUnauthenticated,
	-31403: sdk.ErrUnauthorized,
}

func (e *rpcError) err() error {
	target, ok := errorCodes[e.Code]
	if !ok {
		target = sdk.ErrInternalError
	}
	return fmt.Errorf("a2a: %w (code %d): %s", target, e.Code, e.Message)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) post(ctx context.Context, method string, params any, accept string) (*http.Response, error) {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: fw.NewID(), Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("a2a: marshal %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("a2a: %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set(sdk.SvcParamVersion, string(sdk.Version))
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("a2a: %s: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("a2a: %s: the peer answered %s", method, resp.Status)
	}
	return resp, nil
}

// Call sends one JSON-RPC request and decodes the result into result.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	resp, err := c.post(ctx, method, params, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("a2a: decode %s response: %w", method, err)
	}
	if r.Error != nil {
		return r.Error.err()
	}
	if result == nil || len(r.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.Result, result); err != nil {
		return fmt.Errorf("a2a: decode %s result: %w", method, err)
	}
	return nil
}

// Stream sends one JSON-RPC request that answers with server-sent events and
// yields the event of each one. A JSON-RPC error ends the stream.
func (c *Client) Stream(ctx context.Context, method string, params any) iter.Seq2[sdk.Event, error] {
	return func(yield func(sdk.Event, error) bool) {
		resp, err := c.post(ctx, method, params, "text/event-stream")
		if err != nil {
			yield(nil, err)
			return
		}
		defer resp.Body.Close()
		if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
			// An error before the stream starts comes back as plain JSON.
			var r rpcResponse
			if err := json.NewDecoder(resp.Body).Decode(&r); err == nil && r.Error != nil {
				yield(nil, r.Error.err())
				return
			}
			yield(nil, fmt.Errorf("a2a: %s: the peer answered %q, not an event stream", method, mt))
			return
		}
		for data, err := range sseData(resp.Body) {
			if err != nil {
				yield(nil, fmt.Errorf("a2a: %s stream: %w", method, err))
				return
			}
			var r rpcResponse
			if err := json.Unmarshal(data, &r); err != nil {
				yield(nil, fmt.Errorf("a2a: decode %s event: %w", method, err))
				return
			}
			if r.Error != nil {
				yield(nil, r.Error.err())
				return
			}
			var sr sdk.StreamResponse
			if err := json.Unmarshal(r.Result, &sr); err != nil {
				yield(nil, fmt.Errorf("a2a: decode %s event: %w", method, err))
				return
			}
			if !yield(sr.Event, nil) {
				return
			}
		}
	}
}

// sseData yields the data of each server-sent event. Lines of one event join
// with a newline. Comments, such as keep-alive lines, are skipped.
func sseData(body io.Reader) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
		var ev bytes.Buffer
		flush := func() bool {
			if ev.Len() == 0 {
				return true
			}
			data := bytes.Clone(ev.Bytes())
			ev.Reset()
			return yield(data, nil)
		}
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if !flush() {
					return
				}
				continue
			}
			v, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			if ev.Len() > 0 {
				ev.WriteByte('\n')
			}
			ev.WriteString(strings.TrimPrefix(v, " "))
		}
		if err := sc.Err(); err != nil {
			yield(nil, err)
			return
		}
		flush()
	}
}

// SendMessage calls SendMessage. The result is a *a2a.Task or a *a2a.Message.
func (c *Client) SendMessage(ctx context.Context, req *sdk.SendMessageRequest) (sdk.Event, error) {
	var sr sdk.StreamResponse
	if err := c.Call(ctx, MethodSendMessage, req, &sr); err != nil {
		return nil, err
	}
	if sr.Event == nil {
		return nil, fmt.Errorf("a2a: SendMessage: %w: empty result", sdk.ErrInvalidAgentResponse)
	}
	return sr.Event, nil
}

// SendStreamingMessage calls SendStreamingMessage.
func (c *Client) SendStreamingMessage(ctx context.Context, req *sdk.SendMessageRequest) iter.Seq2[sdk.Event, error] {
	return c.Stream(ctx, MethodSendStreamingMessage, req)
}

// GetTask calls GetTask.
func (c *Client) GetTask(ctx context.Context, req *sdk.GetTaskRequest) (*sdk.Task, error) {
	var t sdk.Task
	if err := c.Call(ctx, MethodGetTask, req, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// JSONRPCURL returns the URL of the JSON-RPC interface of protocol 1.x on a
// card.
func JSONRPCURL(card *sdk.AgentCard) (string, error) {
	for _, i := range card.SupportedInterfaces {
		if i == nil || i.ProtocolBinding != sdk.TransportProtocolJSONRPC {
			continue
		}
		v := strings.TrimPrefix(string(i.ProtocolVersion), "v")
		if v == "" || v == "1" || strings.HasPrefix(v, "1.") {
			return i.URL, nil
		}
	}
	return "", errors.New("a2a: the agent card lists no JSON-RPC interface for protocol 1.x")
}
