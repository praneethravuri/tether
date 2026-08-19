package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/praneethravuri/tether/internal/protocol"
)

func TestRetainedCommandHelpStatesJSONIsDefault(t *testing.T) {
	commands := map[string]func() *cobra.Command{
		"tether": newRootCmd,
		"send":   newSendCmd,
		"inbox":  newInboxCmd,
		"wait":   newWaitCmd,
		"ls":     newLsCmd,
		"claims": newClaimsCmd,
		"doctor": newDoctorCmd,
	}

	for name, build := range commands {
		t.Run(name, func(t *testing.T) {
			r := mustRun(t, build(), "", "--help")
			requireContains(t, r.stdout, "Output is JSON by default.", "help")
		})
	}
}

func TestRetainedCommandsEmitJSONAndWireRequests(t *testing.T) {
	const workspace = "contract-workspace"

	tests := []struct {
		name        string
		build       func() *cobra.Command
		args        []string
		responses   map[string]any
		method      string
		registers   bool
		assertParam func(t *testing.T, request recorded)
		assertJSON  func(t *testing.T, output string)
		setup       func(t *testing.T)
	}{
		{
			name:      "register",
			build:     newRegisterCmd,
			args:      []string{"sender"},
			responses: map[string]any{protocol.MethodRegister: protocol.RegisterResult{Address: "sender@contract-workspace", Name: "sender", Created: true}},
			method:    protocol.MethodRegister,
			assertParam: func(t *testing.T, request recorded) {
				params := decodeParams[protocol.RegisterParams](t, request)
				if params.Name != "sender" || params.Workspace != workspace || params.PID <= 0 {
					t.Fatalf("register params = %+v", params)
				}
			},
			assertJSON: func(t *testing.T, output string) {
				var result protocol.RegisterResult
				unmarshalJSON(t, output, &result)
				if result.Address != "sender@contract-workspace" || !result.Created {
					t.Fatalf("register result = %+v", result)
				}
			},
		},
		{
			name:  "send",
			build: newSendCmd,
			args:  []string{"recipient", "handoff body"},
			responses: map[string]any{
				protocol.MethodRegister: protocol.RegisterResult{Name: "sender"},
				protocol.MethodSend:     protocol.SendResult{MessageID: "message-1", RecipientState: "working"},
			},
			method:    protocol.MethodSend,
			registers: true,
			assertParam: func(t *testing.T, request recorded) {
				params := decodeParams[protocol.SendParams](t, request)
				if params.FromName != "sender" || params.FromWorkspace != workspace ||
					params.ToName != "recipient" || params.ToWorkspace != workspace ||
					params.Body != "handoff body" || params.Kind != kindNote || params.FromSession == "" {
					t.Fatalf("send params = %+v", params)
				}
			},
			assertJSON: func(t *testing.T, output string) {
				var result protocol.SendResult
				unmarshalJSON(t, output, &result)
				if result.MessageID != "message-1" || result.RecipientState != "working" {
					t.Fatalf("send result = %+v", result)
				}
			},
		},
		{
			name:  "inbox",
			build: newInboxCmd,
			args:  []string{"--peek", "--limit", "7"},
			responses: map[string]any{
				protocol.MethodRegister: protocol.RegisterResult{Name: "sender"},
				protocol.MethodInbox: protocol.InboxResult{Messages: []protocol.MessageView{{
					ID: "message-1", From: "other@contract-workspace", To: "sender@contract-workspace", Kind: kindHandoff, Body: "handoff body",
				}}, Pending: 1},
			},
			method:    protocol.MethodInbox,
			registers: true,
			assertParam: func(t *testing.T, request recorded) {
				params := decodeParams[protocol.InboxParams](t, request)
				if params.Name != "sender" || params.Workspace != workspace || !params.Peek || params.Replay || params.Limit != 7 || params.Session == "" {
					t.Fatalf("inbox params = %+v", params)
				}
			},
			assertJSON: func(t *testing.T, output string) {
				var result protocol.InboxResult
				unmarshalJSON(t, output, &result)
				if len(result.Messages) != 1 || result.Messages[0].Body != "handoff body" || result.Pending != 1 {
					t.Fatalf("inbox result = %+v", result)
				}
			},
		},
		{
			name:  "wait",
			build: newWaitCmd,
			args:  []string{"--timeout", "3s"},
			responses: map[string]any{
				protocol.MethodRegister: protocol.RegisterResult{Name: "sender"},
				protocol.MethodWait:     protocol.WaitResult{Pending: 1},
			},
			method:    protocol.MethodWait,
			registers: true,
			assertParam: func(t *testing.T, request recorded) {
				params := decodeParams[protocol.WaitParams](t, request)
				if params.Name != "sender" || params.Workspace != workspace || params.TimeoutMS != 3_000 || params.Session == "" {
					t.Fatalf("wait params = %+v", params)
				}
			},
			assertJSON: func(t *testing.T, output string) {
				var result protocol.WaitResult
				unmarshalJSON(t, output, &result)
				if result.Pending != 1 || result.TimedOut {
					t.Fatalf("wait result = %+v", result)
				}
			},
		},
		{
			name:      "ls",
			build:     newLsCmd,
			responses: map[string]any{protocol.MethodLs: protocol.LsResult{Agents: []protocol.AgentView{{Name: "sender", Workspace: workspace, Address: "sender@contract-workspace"}}}},
			method:    protocol.MethodLs,
			assertParam: func(t *testing.T, request recorded) {
				params := decodeParams[protocol.LsParams](t, request)
				if params.Workspace != workspace || params.Name != "" {
					t.Fatalf("ls params = %+v", params)
				}
			},
			assertJSON: func(t *testing.T, output string) {
				var result protocol.LsResult
				unmarshalJSON(t, output, &result)
				if len(result.Agents) != 1 || result.Agents[0].Address != "sender@contract-workspace" {
					t.Fatalf("ls result = %+v", result)
				}
			},
		},
		{
			name:      "claim",
			build:     newClaimCmd,
			args:      []string{"src/main.go", "--holder", "editing"},
			responses: map[string]any{protocol.MethodClaim: protocol.ClaimResult{LeaseID: "lease-1", Holder: "editing", ExpiresAt: "2030-01-01T00:00:00Z"}},
			method:    protocol.MethodClaim,
			assertParam: func(t *testing.T, request recorded) {
				params := decodeParams[protocol.ClaimParams](t, request)
				if params.Workspace != workspace || params.Key != "src/main.go" || params.Holder != "editing" || params.OwnerPID <= 0 {
					t.Fatalf("claim params = %+v", params)
				}
			},
			assertJSON: func(t *testing.T, output string) {
				var result protocol.ClaimResult
				unmarshalJSON(t, output, &result)
				if result.LeaseID != "lease-1" || result.Holder != "editing" {
					t.Fatalf("claim result = %+v", result)
				}
			},
		},
		{
			name:      "release",
			build:     newReleaseCmd,
			args:      []string{"src/main.go", "--if-claim-id", "lease-1"},
			responses: map[string]any{protocol.MethodRelease: protocol.ReleaseResult{}},
			method:    protocol.MethodRelease,
			assertParam: func(t *testing.T, request recorded) {
				params := decodeParams[protocol.ReleaseParams](t, request)
				if params.Workspace != workspace || params.Key != "src/main.go" || params.LeaseID != "lease-1" {
					t.Fatalf("release params = %+v", params)
				}
			},
			assertJSON: func(t *testing.T, output string) {
				var result protocol.ReleaseResult
				unmarshalJSON(t, output, &result)
			},
		},
		{
			name:      "claims",
			build:     newClaimsCmd,
			responses: map[string]any{protocol.MethodClaims: protocol.ClaimsResult{Claims: []protocol.ClaimView{{Workspace: workspace, Key: "src/main.go", Status: "held"}}}},
			method:    protocol.MethodClaims,
			assertParam: func(t *testing.T, request recorded) {
				params := decodeParams[protocol.ClaimsParams](t, request)
				if params.Workspace != workspace {
					t.Fatalf("claims params = %+v", params)
				}
			},
			assertJSON: func(t *testing.T, output string) {
				var result protocol.ClaimsResult
				unmarshalJSON(t, output, &result)
				if len(result.Claims) != 1 || result.Claims[0].Key != "src/main.go" {
					t.Fatalf("claims result = %+v", result)
				}
			},
		},
		{
			name:      "doctor",
			build:     newDoctorCmd,
			responses: map[string]any{protocol.MethodLs: protocol.LsResult{Agents: []protocol.AgentView{{Name: "sender", Workspace: workspace}}}},
			method:    protocol.MethodLs,
			setup: func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
			},
			assertParam: func(t *testing.T, request recorded) {
				params := decodeParams[protocol.LsParams](t, request)
				if params.Workspace != workspace {
					t.Fatalf("doctor ls params = %+v", params)
				}
			},
			assertJSON: func(t *testing.T, output string) {
				var result doctorReport
				unmarshalJSON(t, output, &result)
				if !result.DaemonRunning || len(result.Agents) != 1 || result.Workspace != workspace {
					t.Fatalf("doctor result = %+v", result)
				}
			},
		},
		{
			name:      "bare tether",
			build:     newRootCmd,
			responses: map[string]any{protocol.MethodLs: protocol.LsResult{Agents: []protocol.AgentView{{Name: "sender", Workspace: workspace, Address: "sender@contract-workspace"}}}},
			method:    protocol.MethodLs,
			assertParam: func(t *testing.T, request recorded) {
				params := decodeParams[protocol.LsParams](t, request)
				if params.Workspace != workspace || params.Name != "" {
					t.Fatalf("bare tether ls params = %+v", params)
				}
			},
			assertJSON: func(t *testing.T, output string) {
				var result protocol.LsResult
				unmarshalJSON(t, output, &result)
				if len(result.Agents) != 1 || result.Agents[0].Name != "sender" {
					t.Fatalf("bare tether result = %+v", result)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setIdentity(t, "sender", workspace)
			if tc.setup != nil {
				tc.setup(t)
			}
			d := newFakeDaemon(t, resultHandler(tc.responses))

			r := mustRun(t, tc.build(), "", tc.args...)
			var request recorded
			if tc.registers {
				request = d.registerThen(t, tc.method)
			} else {
				request = d.only(t, tc.method)
			}
			tc.assertParam(t, request)
			tc.assertJSON(t, r.stdout)
		})
	}
}

func TestFleetCommandsAllIgnoreWorkspace(t *testing.T) {
	tests := []struct {
		name      string
		build     func() *cobra.Command
		method    string
		response  any
		workspace func(t *testing.T, request recorded) string
	}{
		{
			name:     "ls",
			build:    newLsCmd,
			method:   protocol.MethodLs,
			response: protocol.LsResult{},
			workspace: func(t *testing.T, request recorded) string {
				return decodeParams[protocol.LsParams](t, request).Workspace
			},
		},
		{
			name:     "claims",
			build:    newClaimsCmd,
			method:   protocol.MethodClaims,
			response: protocol.ClaimsResult{},
			workspace: func(t *testing.T, request recorded) string {
				return decodeParams[protocol.ClaimsParams](t, request).Workspace
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDaemon(t, resultHandler(map[string]any{tc.method: tc.response}))
			mustRun(t, tc.build(), "", "--all", "--workspace", "one-workspace")
			if got := tc.workspace(t, d.only(t, tc.method)); got != "" {
				t.Fatalf("workspace with --all = %q, want every workspace", got)
			}
		})
	}
}

func TestWaitRoundsPositiveDurationUpToOneMillisecond(t *testing.T) {
	setIdentity(t, "sender", "contract-workspace")
	d := newFakeDaemon(t, resultHandler(map[string]any{
		protocol.MethodRegister: protocol.RegisterResult{Name: "sender"},
		protocol.MethodWait:     protocol.WaitResult{TimedOut: true},
	}))

	r := run(t, newWaitCmd(), "", "--timeout", "1ns")
	if got := r.exitCode(); got != exitTimeout {
		t.Fatalf("wait exit code = %d, want %d", got, exitTimeout)
	}
	params := decodeParams[protocol.WaitParams](t, d.registerThen(t, protocol.MethodWait))
	if params.TimeoutMS != 1 {
		t.Fatalf("wait timeout_ms = %d, want 1", params.TimeoutMS)
	}
}

func TestDoctorReportsNoDaemonWhenSocketCannotResolve(t *testing.T) {
	t.Setenv("TETHER_SOCK", "")
	t.Setenv("TETHER_DB", "")
	t.Setenv("TETHER_WORKSPACE", "doctor-workspace")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", "")

	r := run(t, newDoctorCmd(), "")
	if got := r.exitCode(); got != exitNoDaemon {
		t.Fatalf("doctor exit code = %d, want %d", got, exitNoDaemon)
	}

	var report doctorReport
	unmarshalJSON(t, r.stdout, &report)
	if report.DaemonRunning {
		t.Fatalf("doctor report = %+v, want daemon_running false", report)
	}
	requireContains(t, report.Error, "cannot work out where the tether socket lives", "doctor error")
}

func resultHandler(results map[string]any) handlerFunc {
	return func(request protocol.Request) protocol.Response {
		result, ok := results[request.Method]
		if !ok {
			return protocol.Fail(request.ID, protocol.CodeBadRequest, "unexpected method "+request.Method)
		}
		return protocol.OK(request.ID, result)
	}
}

func TestShellAgentsStaySeparate(t *testing.T) {
	run := startTestTether(t)
	workspace := "same-shell-workspace"

	run("", "register", "sender", "--workspace", workspace)
	run("", "register", "recipient", "--workspace", workspace)
	run("", "register", "sender", "--workspace", workspace)

	var listed protocol.LsResult
	unmarshalJSON(t, run("", "ls", "--workspace", workspace), &listed)
	if len(listed.Agents) != 2 || listed.Agents[0].Name != "recipient" || listed.Agents[1].Name != "sender" {
		t.Fatalf("registered agents = %+v, want sender and recipient", listed.Agents)
	}
}

func TestShellHandoffIdentity(t *testing.T) {
	run := startTestTether(t)
	workspace := "plain-shell-handoff"

	run("", "register", "sender", "--workspace", workspace)
	run("", "register", "recipient", "--workspace", workspace)

	var sent protocol.SendResult
	unmarshalJSON(t, run("", "send", "recipient", "--as", "sender", "--workspace", workspace,
		"--kind", "handoff", "handoff body"), &sent)
	if sent.MessageID == "" {
		t.Fatalf("send result has no message id: %+v", sent)
	}

	var waited protocol.WaitResult
	unmarshalJSON(t, run("", "wait", "--as", "recipient", "--workspace", workspace,
		"--timeout", "3s"), &waited)
	if waited.Pending != 1 || waited.TimedOut {
		t.Fatalf("wait result = %+v, want one pending handoff", waited)
	}

	var inbox protocol.InboxResult
	unmarshalJSON(t, run("", "inbox", "--as", "recipient", "--workspace", workspace), &inbox)
	if len(inbox.Messages) != 1 || inbox.Messages[0].ID != sent.MessageID || inbox.Messages[0].Body != "handoff body" {
		t.Fatalf("inbox result = %+v", inbox)
	}
}

// TestShellImplicitHandoff keeps the unnamed plain-shell session stable across
// implicit registration and its real send, wait, and inbox requests.
func TestShellImplicitHandoff(t *testing.T) {
	run := startTestTether(t)
	workspace := "implicit-shell-handoff"

	run("", "register", "recipient", "--workspace", workspace)

	var sent protocol.SendResult
	unmarshalJSON(t, run("", "send", "recipient", "--workspace", workspace,
		"--kind", "handoff", "first handoff"), &sent)
	if sent.MessageID == "" {
		t.Fatalf("send result has no message id: %+v", sent)
	}

	var listed protocol.LsResult
	unmarshalJSON(t, run("", "ls", "--workspace", workspace), &listed)
	var sender string
	for _, agent := range listed.Agents {
		if agent.Name != "recipient" {
			sender = agent.Name
			break
		}
	}
	if sender == "" {
		t.Fatalf("implicit sender missing from agents: %+v", listed.Agents)
	}

	run("", "send", sender, "--as", "recipient", "--workspace", workspace,
		"--kind", "answer", "reply handoff")

	var waited protocol.WaitResult
	unmarshalJSON(t, run("", "wait", "--workspace", workspace, "--timeout", "3s"), &waited)
	if waited.Pending != 1 || waited.TimedOut {
		t.Fatalf("wait result = %+v, want one pending reply", waited)
	}

	var inbox protocol.InboxResult
	unmarshalJSON(t, run("", "inbox", "--workspace", workspace), &inbox)
	if len(inbox.Messages) != 1 || inbox.Messages[0].Body != "reply handoff" {
		t.Fatalf("inbox result = %+v", inbox)
	}
}

func TestRealProcessHandoff(t *testing.T) {
	run := startTestTether(t)
	workspace := "handoff-workspace"

	var sender protocol.RegisterResult
	unmarshalJSON(t, run("sender-session", "register", "sender", "--workspace", workspace), &sender)
	if sender.Name != "sender" || sender.Address != "sender@handoff-workspace" {
		t.Fatalf("sender registration = %+v", sender)
	}

	var recipient protocol.RegisterResult
	unmarshalJSON(t, run("recipient-session", "register", "recipient", "--workspace", workspace), &recipient)
	if recipient.Name != "recipient" || recipient.Address != "recipient@handoff-workspace" {
		t.Fatalf("recipient registration = %+v", recipient)
	}

	var sent protocol.SendResult
	unmarshalJSON(t, run("sender-session", "send", "recipient", "--workspace", workspace,
		"--kind", "handoff", "handoff body"), &sent)
	if sent.MessageID == "" {
		t.Fatalf("send result has no message id: %+v", sent)
	}

	var waited protocol.WaitResult
	unmarshalJSON(t, run("recipient-session", "wait", "--as", "recipient", "--workspace", workspace,
		"--timeout", "3s"), &waited)
	if waited.Pending != 1 || waited.TimedOut {
		t.Fatalf("wait result = %+v, want one pending handoff", waited)
	}

	var inbox protocol.InboxResult
	unmarshalJSON(t, run("recipient-session", "inbox", "--as", "recipient", "--workspace", workspace), &inbox)
	if len(inbox.Messages) != 1 || inbox.Messages[0].ID != sent.MessageID ||
		inbox.Messages[0].Kind != kindHandoff || inbox.Messages[0].Body != "handoff body" || inbox.Cleared != 1 {
		t.Fatalf("inbox result = %+v", inbox)
	}
}

func startTestTether(t *testing.T) func(string, ...string) string {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "tether")
	// #nosec G204 -- the test controls the go tool and its temporary output path.
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Env = os.Environ()
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build tether binary: %v\n%s", err, output)
	}

	socket := filepath.Join(dir, "sock")
	database := filepath.Join(dir, "tether.db")
	// #nosec G204 -- binary was built by this test in t.TempDir.
	daemon := exec.Command(binary, "start")
	daemon.Env = handoffEnv(socket, database, dir, "")
	if err := daemon.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	t.Cleanup(func() {
		if daemon.Process == nil {
			return
		}
		if err := daemon.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("interrupt daemon: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- daemon.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("wait for daemon: %v", err)
			}
		case <-time.After(5 * time.Second):
			if err := daemon.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("kill daemon: %v", err)
			}
			<-done
		}
	})
	waitForSocket(t, socket)

	return func(session string, args ...string) string {
		t.Helper()
		// #nosec G204 -- binary was built by this test in t.TempDir; args are literals below.
		cmd := exec.Command(binary, args...)
		cmd.Env = handoffEnv(socket, database, dir, session)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tether %s: %v\n%s", strings.Join(args, " "), err, output)
		}
		return string(output)
	}
}

func handoffEnv(socket, database, home, session string) []string {
	removed := map[string]bool{
		"TETHER_SOCK":            true,
		"TETHER_DB":              true,
		"TETHER_WORKSPACE":       true,
		"TETHER_SESSION_ID":      true,
		"CLAUDE_CODE_SESSION_ID": true,
		"CLAUDECODE":             true,
		"GEMINI_SESSION_ID":      true,
		"COPILOT_HOME":           true,
		"COPILOT_SESSION_ID":     true,
		"OPENCODE_SESSION_ID":    true,
		"AMP_THREAD_ID":          true,
		"HOME":                   true,
	}
	env := make([]string, 0, len(os.Environ())+4)
	for _, pair := range os.Environ() {
		key, _, _ := strings.Cut(pair, "=")
		if !removed[key] && !strings.HasPrefix(key, "OPENCODE_") {
			env = append(env, pair)
		}
	}
	return append(env,
		"TETHER_SOCK="+socket,
		"TETHER_DB="+database,
		"TETHER_SESSION_ID="+session,
		"HOME="+home,
	)
}

func waitForSocket(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socket, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("daemon did not start listening on %s", socket)
}
