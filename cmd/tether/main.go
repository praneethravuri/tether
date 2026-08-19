// Command tether is the CLI for cross-harness agent messaging: one registry
// and inbox that any coding-agent harness can drive from the shell.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/praneethravuri/tether/internal/protocol"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags)" ./cmd/tether
var version = "dev"

// Exit codes. These are part of the CLI's contract: scripts and agents branch
// on them, so they must never be reused for a different meaning.
const (
	// exitOK means the command did what it was asked to do.
	exitOK = 0
	// exitGeneral is any failure without a more specific code.
	exitGeneral = 1
	// exitNoDaemon means the daemon could not be reached.
	exitNoDaemon = 3
	// exitTimeout means `tether wait` returned with no mail.
	exitTimeout = 4
	// exitConflict means the request collided with existing state, most often
	// a name that is already registered in the workspace.
	exitConflict = 5
)

// exitError carries the process exit code a failure should produce.
//
// cobra's Execute reports a plain error, so commands return an *exitError and
// main unwraps it with errors.As to choose the code. A nil err means the
// command has already told the user what happened on stdout and main should
// exit quietly (this is how `wait` reports a timeout).
type exitError struct {
	code int
	err  error
}

// Error implements error.
func (e *exitError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.err == nil {
		return fmt.Sprintf("exit status %d", e.code)
	}
	return e.err.Error()
}

// Unwrap exposes the underlying cause so errors.As keeps working through it.
func (e *exitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// failf builds an *exitError with a formatted message.
func failf(code int, format string, a ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, a...)}
}

// fail wraps err so it exits with code.
func fail(code int, err error) error {
	return &exitError{code: code, err: err}
}

// silentExit exits with code without printing anything further; the command
// has already rendered its result.
func silentExit(code int) error {
	return &exitError{code: code}
}

// exitCodeFor maps an error returned by a command to a process exit code.
func exitCodeFor(err error) int {
	if err == nil {
		return exitOK
	}

	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}

	// Safety net: a daemon-side conflict always exits 5 even if a command
	// forgot to translate it.
	var pe *protocol.Error
	if errors.As(err, &pe) && pe.Code == protocol.CodeConflict {
		return exitConflict
	}

	return exitGeneral
}

// errorMessage returns what main should print to stderr, or "" when the
// command has already reported the outcome itself.
func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	var ee *exitError
	if errors.As(err, &ee) && ee.err == nil {
		return ""
	}
	return err.Error()
}

func main() {
	err := newRootCmd().Execute()
	if err == nil {
		return
	}
	if msg := errorMessage(err); msg != "" {
		fmt.Fprintln(os.Stderr, "tether: "+msg)
	}
	os.Exit(exitCodeFor(err))
}

const rootLong = `tether is a local message bus for coding agents.

Agents register a name inside a workspace derived from the shared Git root
(or the current directory outside Git), unless $TETHER_WORKSPACE overrides it.
They address each other as name@workspace. A bare name uses the current
workspace.

Typical session:

  tether register frontend          # claim a name in this workspace
  tether ls                         # see who else is here
  tether send backend --as frontend "..." # send a message
  tether wait --as frontend --timeout 60s  # block until mail arrives
  tether inbox --as frontend                # read it -- this also clears it
  tether inbox --as frontend --peek         # look without clearing

Message bodies should be passed with --body-file (use "-" for stdin) whenever
they contain quotes, backticks, newlines or $ so the shell cannot mangle them.

Output is JSON by default. Running tether with no arguments lists the agents
in the current workspace. Run "tether start" to run the daemon itself in the
foreground -- daemon-facing commands start it automatically when needed;
` + "`tether doctor`" + ` only reports its status and never starts one.

Exit codes:
  0  success
  1  general error
  3  no daemon running
  4  wait timed out, or send found no recipient
  5  conflict (for example, the name is already taken)`

// newRootCmd's bare form lists the current workspace (see runRoot); NoArgs
// makes a typo'd subcommand fail loudly instead of being silently ignored.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "tether",
		Short:         "Local message bus for coding agents",
		Long:          rootLong,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRoot(cmd)
		},
	}
	root.SetFlagErrorFunc(flagErrorFunc)

	root.AddCommand(
		newVersionCmd(),
		newStartCmd(),
		newRegisterCmd(),
		newSendCmd(),
		newInboxCmd(),
		newWaitCmd(),
		newLsCmd(),
		newClaimCmd(),
		newReleaseCmd(),
		newClaimsCmd(),
		newDoctorCmd(),
	)

	return root
}

func newVersionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version)
			return err
		},
	}
	return quiet(cmd)
}
