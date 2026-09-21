package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestWireFormat_RoundTrip(t *testing.T) {
	// bytes.Buffer acts like a network connection in memory
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	decoder := json.NewDecoder(&buf)

	params, err := json.Marshal(RegisterParams{
		Name:      "alice",
		Workspace: "tether",
		Harness:   "claude-code",
		SessionID: "sess-1",
		Cwd:       "/tmp/tether",
		PID:       4242,
	})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}

	req := Request{
		ID:     "msg-123",
		Method: MethodRegister,
		Params: params,
	}

	// 1. Encode into the buffer
	if err := encoder.Encode(req); err != nil {
		t.Fatalf("failed to encode: %v", err)
	}

	// 2. Decode out of the buffer
	var decodedReq Request
	if err := decoder.Decode(&decodedReq); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	// 3. Verify the data survived the round trip
	if decodedReq.ID != req.ID {
		t.Errorf("ID got %q, want %q", decodedReq.ID, req.ID)
	}
	if decodedReq.Method != req.Method {
		t.Errorf("Method got %q, want %q", decodedReq.Method, req.Method)
	}

	var decodedParams RegisterParams
	if err := json.Unmarshal(decodedReq.Params, &decodedParams); err != nil {
		t.Fatalf("failed to decode params: %v", err)
	}
	if decodedParams.Name != "alice" || decodedParams.PID != 4242 {
		t.Errorf("params got %+v", decodedParams)
	}
}

// TestRequest_RoundTripAllMethods pushes one request per method through
// Marshal/Unmarshal to make sure nothing in the envelope is lossy.
func TestRequest_RoundTripAllMethods(t *testing.T) {
	cases := []struct {
		method string
		params any
	}{
		{MethodRegister, RegisterParams{Name: "a", Workspace: "w", Harness: "claude-code", SessionID: "s", Cwd: "/c", PID: 1}},
		{MethodSend, SendParams{FromName: "a", FromWorkspace: "w", FromSession: "s", ToName: "b", ToWorkspace: "w", Kind: "ask", Body: "hi", ReplyTo: "m1"}},
		{MethodInbox, InboxParams{Name: "a", Workspace: "w", Limit: 10, Replay: true}},
		{MethodWait, WaitParams{Name: "a", Workspace: "w", TimeoutMS: 5000}},
		{MethodLs, LsParams{Workspace: "w"}},
		{MethodLs, LsParams{Name: "a", Workspace: "w"}},
	}

	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			raw, err := json.Marshal(tc.params)
			if err != nil {
				t.Fatalf("marshal params: %v", err)
			}
			req := Request{ID: "id-" + tc.method, Method: tc.method, Params: raw}

			data, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}

			var got Request
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal request: %v", err)
			}
			if got.ID != req.ID || got.Method != req.Method {
				t.Errorf("envelope mismatch: got %+v want %+v", got, req)
			}
			if !json.Valid(got.Params) {
				t.Errorf("params did not survive as valid JSON: %s", got.Params)
			}
			if string(got.Params) != string(raw) {
				t.Errorf("params got %s, want %s", got.Params, raw)
			}
		})
	}
}

// TestResponse_RoundTripTypedResult checks the full daemon->CLI path: build a
// response with OK, serialise it, read it back, and decode the typed result.
func TestResponse_RoundTripTypedResult(t *testing.T) {
	delivered := "2026-07-28T10:00:00Z"
	want := InboxResult{Messages: []MessageView{
		{
			ID:          "m1",
			ThreadID:    "t1",
			ReplyTo:     "",
			From:        "alice@tether",
			To:          "bob@tether",
			Kind:        "ask",
			Body:        "ping",
			CreatedAt:   "2026-07-28T09:59:00Z",
			DeliveredAt: &delivered,
		},
	}}

	resp := OK("req-1", want)
	if resp.Error != nil {
		t.Fatalf("OK() set Error: %v", resp.Error)
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}

	var got Response
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.ID != "req-1" {
		t.Errorf("ID got %q, want %q", got.ID, "req-1")
	}
	if got.Error != nil {
		t.Fatalf("Error should be nil, got %v", got.Error)
	}

	var result InboxResult
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("messages got %d, want 1", len(result.Messages))
	}
	m := result.Messages[0]
	if m.ID != "m1" || m.From != "alice@tether" || m.Body != "ping" {
		t.Errorf("message mismatch: %+v", m)
	}
	if m.DeliveredAt == nil || *m.DeliveredAt != delivered {
		t.Errorf("DeliveredAt got %v, want %q", m.DeliveredAt, delivered)
	}
	if m.AckedAt != nil {
		t.Errorf("AckedAt got %v, want nil", *m.AckedAt)
	}
}

func TestOK_SetsResultLeavesErrorNil(t *testing.T) {
	resp := OK("id-1", SendResult{MessageID: "m1"})
	if resp.Error != nil {
		t.Fatalf("Error got %v, want nil", resp.Error)
	}
	if len(resp.Result) == 0 {
		t.Fatal("Result is empty")
	}

	var got SendResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got.MessageID != "m1" {
		t.Errorf("MessageID got %q, want %q", got.MessageID, "m1")
	}
}

func TestOK_NilResultIsEmpty(t *testing.T) {
	resp := OK("id-1", nil)
	if len(resp.Result) != 0 {
		t.Errorf("Result got %s, want empty", resp.Result)
	}
	if resp.Error != nil {
		t.Errorf("Error got %v, want nil", resp.Error)
	}
}

func TestOK_UnmarshalableValueFailsInternal(t *testing.T) {
	// A channel cannot be marshalled; OK must degrade, never panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("OK() panicked: %v", r)
		}
	}()

	resp := OK("id-1", make(chan int))

	if resp.Error == nil {
		t.Fatal("expected an Error response, got nil")
	}
	if resp.Error.Code != CodeInternal {
		t.Errorf("code got %d, want %d", resp.Error.Code, CodeInternal)
	}
	if resp.Error.Message == "" {
		t.Error("expected a non-empty error message")
	}
	if len(resp.Result) != 0 {
		t.Errorf("Result got %s, want empty", resp.Result)
	}
	if resp.ID != "id-1" {
		t.Errorf("ID got %q, want %q", resp.ID, "id-1")
	}

	// And it must still be serialisable so the daemon can write it back.
	if _, err := json.Marshal(resp); err != nil {
		t.Fatalf("failed to marshal degraded response: %v", err)
	}
}

func TestFail_SetsErrorLeavesResultEmpty(t *testing.T) {
	resp := Fail("id-2", CodeNotFound, "no such agent")
	if len(resp.Result) != 0 {
		t.Errorf("Result got %s, want empty", resp.Result)
	}
	if resp.Error == nil {
		t.Fatal("Error is nil")
	}
	if resp.Error.Code != CodeNotFound {
		t.Errorf("code got %d, want %d", resp.Error.Code, CodeNotFound)
	}
	if resp.Error.Message != "no such agent" {
		t.Errorf("message got %q", resp.Error.Message)
	}
}

func TestError_ImplementsErrorInterface(t *testing.T) {
	var err error = &Error{Code: CodeConflict, Message: "name taken"}

	if got := err.Error(); !strings.Contains(got, "409") || !strings.Contains(got, "name taken") {
		t.Errorf("Error() got %q, want it to mention the code and message", got)
	}

	// It must be usable with errors.As, which is how the CLI will map codes
	// onto exit statuses.
	wrapped := errors.New("rpc failed: " + err.Error())
	if wrapped.Error() == "" {
		t.Error("unexpected empty wrapped error")
	}

	var target *Error
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed to extract *Error")
	}
	if target.Code != CodeConflict {
		t.Errorf("code got %d, want %d", target.Code, CodeConflict)
	}

	// A nil receiver must not panic.
	var nilErr *Error
	if got := nilErr.Error(); got == "" {
		t.Error("nil *Error produced an empty string")
	}
}

func TestResponse_OmitEmpty(t *testing.T) {
	t.Run("no error key when Error is nil", func(t *testing.T) {
		data, err := json.Marshal(OK("id-1", RegisterResult{Address: "alice@tether", Created: true}))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["error"]; ok {
			t.Errorf("unexpected \"error\" key in %s", data)
		}
		if _, ok := m["result"]; !ok {
			t.Errorf("missing \"result\" key in %s", data)
		}
	})

	t.Run("no result key when Result is empty", func(t *testing.T) {
		data, err := json.Marshal(Fail("id-1", CodeBadRequest, "bad"))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["result"]; ok {
			t.Errorf("unexpected \"result\" key in %s", data)
		}
		if _, ok := m["error"]; !ok {
			t.Errorf("missing \"error\" key in %s", data)
		}
	})

	t.Run("no params key when Params is empty", func(t *testing.T) {
		data, err := json.Marshal(Request{ID: "id-1", Method: MethodLs})
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["params"]; ok {
			t.Errorf("unexpected \"params\" key in %s", data)
		}
	})
}

// TestNewlineDelimitedFraming mirrors how the daemon reads a reused
// connection: many requests streamed into one buffer, decoded in order.
func TestNewlineDelimitedFraming(t *testing.T) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)

	methods := []string{MethodRegister, MethodSend, MethodInbox, MethodWait, MethodLs}
	for i, m := range methods {
		req := Request{ID: string(rune('a' + i)), Method: m}
		if err := enc.Encode(req); err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}
	}

	// json.Encoder writes one object per line.
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != len(methods) {
		t.Fatalf("got %d lines, want %d: %q", len(lines), len(methods), buf.String())
	}
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Errorf("line %d is not standalone valid JSON: %q", i, line)
		}
	}

	dec := json.NewDecoder(&buf)
	for i, m := range methods {
		var got Request
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		if got.Method != m {
			t.Errorf("frame %d method got %q, want %q", i, got.Method, m)
		}
		if want := string(rune('a' + i)); got.ID != want {
			t.Errorf("frame %d id got %q, want %q", i, got.ID, want)
		}
	}

	// The stream is exhausted.
	var extra Request
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF after the last frame, got %v", err)
	}
}

func TestDecode_MalformedInputReturnsError(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("decoding malformed input panicked: %v", r)
		}
	}()

	cases := []struct {
		name string
		in   string
	}{
		{"truncated object", `{"id":"1","method":"send"`},
		{"garbage", `not json at all`},
		{"wrong type for id", `{"id":123,"method":"send"}`},
		{"empty", ``},
		{"half a line", "{\"id\":\"1\",\"method\":\"send\"}\n{\"id\":\"2\","},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := json.NewDecoder(strings.NewReader(tc.in))
			var err error
			for {
				var req Request
				if err = dec.Decode(&req); err != nil {
					break
				}
			}
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestDecode_UnknownMethodIsNotADecodeError(t *testing.T) {
	// Routing on an unknown method is the daemon's job (CodeBadRequest); the
	// envelope itself must still decode cleanly.
	var req Request
	if err := json.Unmarshal([]byte(`{"id":"1","method":"teleport"}`), &req); err != nil {
		t.Fatalf("unexpected decode error: %v", err)
	}
	if req.Method != "teleport" {
		t.Errorf("method got %q", req.Method)
	}
}
