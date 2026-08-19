package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/praneethravuri/tether/internal/protocol"
)

func TestVersion(t *testing.T) {
	r := mustRun(t, newRootCmd(), "", "version")
	if strings.TrimSpace(r.stdout) != version {
		t.Fatalf("stdout = %q, want %q", r.stdout, version)
	}
}

// identityBearingCommands builds every command that authenticates or reads
// as a particular agent -- the set both TestEveryCommandHasHelp and the
// unknown-flag test below exercise identically.
var identityBearingCommands = map[string]func() *cobra.Command{
	"register": newRegisterCmd,
	"send":     newSendCmd,
	"inbox":    newInboxCmd,
	"wait":     newWaitCmd,
	"ls":       newLsCmd,
	"claim":    newClaimCmd,
	"doctor":   newDoctorCmd,
}

func TestRootHelpListsEveryCommand(t *testing.T) {
	r := mustRun(t, newRootCmd(), "", "--help")

	for _, want := range []string{
		"start", "register", "send", "inbox", "wait", "ls", "claim", "doctor", "version",
	} {
		requireContains(t, r.stdout, want, "help")
	}

	// The help is the only documentation an agent reads before its first call,
	// so the two things that trip callers up have to be in it.
	requireContains(t, r.stdout, "--body-file", "help")
	requireContains(t, r.stdout, "Exit codes", "help")
}

func TestEveryCommandHasHelp(t *testing.T) {
	for name, build := range identityBearingCommands {
		t.Run(name, func(t *testing.T) {
			cmd := build()
			if cmd.Short == "" {
				t.Fatalf("%s has no short description", name)
			}
			if cmd.Long == "" {
				t.Fatalf("%s has no long description", name)
			}
			if cmd.Example == "" {
				t.Fatalf("%s has no example", name)
			}

			r := mustRun(t, build(), "", "--help")
			requireContains(t, r.stdout, "Usage:", "help")
			requireContains(t, r.stdout, "Examples:", "help")
			requireContains(t, r.stdout, cmd.Name(), "help")
		})
	}
}

// A runtime failure must not dump the usage block: the message says what to do,
// and a wall of flags buries it.
func TestRuntimeFailuresDoNotPrintUsage(t *testing.T) {
	setIdentity(t, "frontend", "storefront")
	noDaemon(t)

	r := run(t, newRootCmd(), "", "ls")
	if r.err == nil {
		t.Fatal("ls succeeded with no daemon")
	}
	requireNotContains(t, r.stdout, "Usage:", "stdout")
	requireNotContains(t, r.stderr, "Usage:", "stderr")
	requireNotContains(t, r.stderr, "Error:", "stderr")
}

func TestRootRoutesToSubcommands(t *testing.T) {
	setIdentity(t, "frontend", "storefront")
	d := newFakeDaemon(t, okHandler(protocol.SendResult{MessageID: "01KROUTED"}))

	r := mustRun(t, newRootCmd(), "", "send", "backend", "hello")

	d.registerThen(t, protocol.MethodSend)
	requireContains(t, r.stdout, "01KROUTED", "stdout")
}

// TestUnknownCommandIsRejected: root.Args = cobra.NoArgs is what makes an
// unrecognised subcommand fail loudly instead of being treated as an ignored
// positional argument to root's own RunE. This is cobra's own out-of-the-box
// "unknown command" behaviour for a NoArgs root with subcommands, verified
// here rather than assumed.
func TestUnknownCommandIsRejected(t *testing.T) {
	r := run(t, newRootCmd(), "", "teleport")
	if r.err == nil {
		t.Fatal("an unknown command was accepted")
	}
	if got := r.exitCode(); got != exitGeneral {
		t.Fatalf("exit code = %d, want %d", got, exitGeneral)
	}
	requireContains(t, r.err.Error(), "unknown command", "error")
	requireNotContains(t, r.stdout, "Usage:", "stdout")
	requireNotContains(t, r.stderr, "Usage:", "stderr")
}

// TestUnknownFlagListsValidFlags is AXI principle 2: an unknown flag must be
// rejected before any daemon call, and the error must name this command's
// real flags inline, so a caller can self-correct in the same turn instead
// of guessing what was silently ignored.
func TestUnknownFlagListsValidFlags(t *testing.T) {
	for name, build := range identityBearingCommands {
		t.Run(name, func(t *testing.T) {
			r := run(t, build(), "", "--this-flag-does-not-exist")
			if r.err == nil {
				t.Fatalf("%s accepted an unknown flag", name)
			}
			requireContains(t, r.err.Error(), "unknown flag: --this-flag-does-not-exist", "error")
			requireContains(t, r.err.Error(), "valid flags for "+name, "error")
			requireContains(t, r.err.Error(), "--workspace", "error")
		})
	}
}

// TestUnknownFlagOnCommandsWithNoCustomFlags covers the commands that
// register nothing of their own: bare tether, start, and version. cobra
// always adds --help by the time flags are parsed, so that is what the
// error lists -- not an empty "(none)" that would contradict --help working.
func TestUnknownFlagOnCommandsWithNoCustomFlags(t *testing.T) {
	cmds := map[string]func() *cobra.Command{
		"tether":  newRootCmd,
		"start":   newStartCmd,
		"version": newVersionCmd,
	}
	for name, build := range cmds {
		t.Run(name, func(t *testing.T) {
			r := run(t, build(), "", "--this-flag-does-not-exist")
			if r.err == nil {
				t.Fatalf("%s accepted an unknown flag", name)
			}
			requireContains(t, r.err.Error(), "unknown flag: --this-flag-does-not-exist", "error")
			requireContains(t, r.err.Error(), "valid flags for "+name+": --help", "error")
		})
	}
}

func TestExitCodeFor(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "no error", err: nil, want: exitOK},
		{name: "plain error", err: errors.New("boom"), want: exitGeneral},
		{name: "explicit code", err: failf(exitNoDaemon, "down"), want: exitNoDaemon},
		{name: "silent exit", err: silentExit(exitTimeout), want: exitTimeout},
		{
			name: "wrapped explicit code",
			err:  errors.Join(errors.New("context"), failf(exitConflict, "taken")),
			want: exitConflict,
		},
		{
			name: "daemon conflict without an explicit code",
			err:  &protocol.Error{Code: protocol.CodeConflict, Message: "taken"},
			want: exitConflict,
		},
		{
			name: "other daemon errors are general failures",
			err:  &protocol.Error{Code: protocol.CodeNotFound, Message: "missing"},
			want: exitGeneral,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCodeFor(tc.err); got != tc.want {
				t.Fatalf("exitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestErrorMessage(t *testing.T) {
	if msg := errorMessage(nil); msg != "" {
		t.Fatalf("errorMessage(nil) = %q, want empty", msg)
	}
	if msg := errorMessage(silentExit(exitTimeout)); msg != "" {
		t.Fatalf("a silent exit would print %q; it should print nothing", msg)
	}
	if msg := errorMessage(failf(exitGeneral, "something broke")); msg != "something broke" {
		t.Fatalf("errorMessage = %q", msg)
	}
}

// The exit error has to keep errors.As working through it, which is what lets
// commands inspect a daemon code after wrapping.
func TestExitErrorUnwraps(t *testing.T) {
	inner := &protocol.Error{Code: protocol.CodeTooLarge, Message: "too big"}
	wrapped := fail(exitGeneral, inner)

	var pe *protocol.Error
	if !errors.As(wrapped, &pe) {
		t.Fatal("errors.As could not reach the protocol error")
	}
	if pe.Code != protocol.CodeTooLarge {
		t.Fatalf("code = %d, want %d", pe.Code, protocol.CodeTooLarge)
	}
	if code, ok := daemonCode(wrapped); !ok || code != protocol.CodeTooLarge {
		t.Fatalf("daemonCode = (%d, %v)", code, ok)
	}
	if _, ok := daemonCode(errors.New("not from the daemon")); ok {
		t.Fatal("daemonCode claimed a plain error came from the daemon")
	}
}

func TestExitErrorNilReceiverIsSafe(t *testing.T) {
	var nilErr *exitError
	if got := nilErr.Error(); got != "<nil>" {
		t.Errorf("nil *exitError.Error() = %q, want %q", got, "<nil>")
	}
	if got := nilErr.Unwrap(); got != nil {
		t.Errorf("nil *exitError.Unwrap() = %v, want nil", got)
	}
}

func TestExitErrorWithoutCauseRendersExitStatus(t *testing.T) {
	err := silentExit(exitTimeout)
	if got := err.Error(); got != "exit status 4" {
		t.Errorf("Error() = %q, want %q", got, "exit status 4")
	}
}
