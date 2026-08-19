package main

import (
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/praneethravuri/tether/internal/kind"
	"github.com/praneethravuri/tether/internal/protocol"
)

// Message kinds are advisory only, re-exported from internal/kind so every
// call site in this package keeps working unchanged.
const (
	kindNote     = kind.Note
	kindHandoff  = kind.Handoff
	kindQuestion = kind.Question
	kindAnswer   = kind.Answer
)

// validKinds is the accepted set, in the order shown in help and errors.
var validKinds = kind.All

const sendLong = `Send a message to another agent.

The recipient is the first argument: name@workspace, or a bare name resolved
against your own workspace. Send to '*' or 'all' to broadcast to every other
registered agent in the workspace (quote '*' so your shell does not glob-
expand it).

The body can be the second positional argument, but pass it with --body-file
whenever it contains quotes, backticks, newlines or $ — the shell will
otherwise mangle it before tether ever sees it. Use --body-file - to read the
body from stdin. What is read is sent byte for byte, with no trimming.

Kinds: note (default), handoff, question, answer. Use --reply-to <message-id>
to thread an answer onto the message it answers.

Output is JSON by default.`

type sendOptions struct {
	identityFlags
	kind     string
	replyTo  string
	bodyFile string
}

func newSendCmd() *cobra.Command {
	var opts sendOptions

	cmd := &cobra.Command{
		Use:   "send <to> [body]",
		Short: "Send a message to another agent",
		Long:  sendLong,
		Example: "  tether send backend \"the API contract changed\"\n" +
			"  tether send backend@storefront --kind handoff --body-file notes.md\n" +
			"  cat report.txt | tether send reviewer --body-file -\n" +
			"  tether send frontend --kind answer --reply-to 01K1QW8Z3M4T7V9XBCDEF2GH --body-file -\n" +
			"  tether send '*' \"heads up, deploying in 5\"\n" +
			"  tether send all \"heads up, deploying in 5\"",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSend(cmd, args, &opts)
		},
	}

	opts.addIdentity(cmd)
	cmd.Flags().StringVar(&opts.kind, "kind", kindNote,
		"message kind: "+strings.Join(validKinds, ", "))
	cmd.Flags().StringVar(&opts.replyTo, "reply-to", "", "id of the message this replies to")
	cmd.Flags().StringVar(&opts.bodyFile, "body-file", "",
		"read the body from this file, or from stdin when set to -")

	return quiet(cmd)
}

// isBroadcastTarget reports whether name is a reserved broadcast marker
// (exact match, not a prefix — "allocator" is not a broadcast).

func runSend(cmd *cobra.Command, args []string, opts *sendOptions) error {
	target, bodyArgs := args[0], args[1:]

	kind, err := normaliseKind(opts.kind)
	if err != nil {
		return err
	}

	fromName, workspace, err := resolveSelf(opts.name, opts.workspace)
	if err != nil {
		return err
	}

	toName, toWorkspace, err := resolveTarget(target, workspace)
	if err != nil {
		return err
	}

	body, err := readBody(cmd, bodyArgs, opts.bodyFile)
	if err != nil {
		return err
	}

	fromName, session, err := ensureRegistered(fromName, workspace)
	if err != nil {
		return err
	}

	params := protocol.SendParams{
		FromName:      fromName,
		FromWorkspace: workspace,
		FromSession:   session,
		ToName:        toName,
		ToWorkspace:   toWorkspace,
		Kind:          kind,
		Body:          body,
		ReplyTo:       strings.TrimSpace(opts.replyTo),
	}

	var res protocol.SendResult
	if err := call(protocol.MethodSend, params, &res); err != nil {
		// "nobody was there" shares wait's timeout exit code, not the general one.
		if code, ok := daemonCode(err); ok && code == protocol.CodeNotFound {
			return fail(exitTimeout, err)
		}
		return err
	}

	out := cmd.OutOrStdout()
	return printJSON(out, res)
}

// normaliseKind validates --kind and returns the canonical spelling.
func normaliseKind(kind string) (string, error) {
	k := strings.ToLower(strings.TrimSpace(kind))
	if k == "" {
		return kindNote, nil
	}
	for _, valid := range validKinds {
		if k == valid {
			return k, nil
		}
	}
	return "", failf(exitGeneral, "unknown --kind %q: use one of %s",
		kind, strings.Join(validKinds, ", "))
}

// readBody resolves the message body from the positional argument or
// --body-file, verbatim with no trimming or interpretation.
func readBody(cmd *cobra.Command, args []string, bodyFile string) (string, error) {
	hasArg := len(args) > 0
	hasFile := bodyFile != ""

	switch {
	case hasArg && hasFile:
		return "", failf(exitGeneral,
			"the body was given twice: pass it either as an argument or with "+
				"--body-file, not both")
	case !hasArg && !hasFile:
		return "", failf(exitGeneral,
			"no message body: pass it as an argument, or with --body-file <path> "+
				"(--body-file - reads stdin, which is the safe way to send text "+
				"containing quotes, backticks or newlines)")
	case hasArg:
		if args[0] == "" {
			return "", failf(exitGeneral, "the message body is empty")
		}
		return args[0], nil
	}

	var (
		raw []byte
		err error
	)
	if bodyFile == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", failf(exitGeneral, "cannot read the message body from stdin: %v", err)
		}
	} else {
		raw, err = os.ReadFile(bodyFile)
		if err != nil {
			return "", failf(exitGeneral, "cannot read the message body from %s: %v",
				bodyFile, err)
		}
	}

	if len(raw) == 0 {
		return "", failf(exitGeneral, "the message body read from %s is empty",
			describeBodySource(bodyFile))
	}

	return string(raw), nil
}

// describeBodySource names where a body came from for error messages.
func describeBodySource(bodyFile string) string {
	if bodyFile == "-" {
		return "stdin"
	}
	return bodyFile
}
