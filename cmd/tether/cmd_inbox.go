package main

import (
	"github.com/spf13/cobra"

	"github.com/praneethravuri/tether/internal/protocol"
)

const inboxLong = `Show the messages waiting for this agent.

By default this drains the inbox: messages are shown and acknowledged in the
same call, so there is no separate step to get wrong. Use --peek to look
without clearing anything, or --replay to see messages an earlier drain
already delivered.

Output is JSON by default.`

type inboxOptions struct {
	identityFlags
	limit  int
	peek   bool
	replay bool
}

func newInboxCmd() *cobra.Command {
	var opts inboxOptions

	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "Show messages waiting for this agent",
		Long:  inboxLong,
		Example: "  tether inbox\n" +
			"  tether inbox --as frontend --limit 10\n" +
			"  tether inbox --peek\n" +
			"  tether inbox --replay",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInbox(cmd, &opts)
		},
	}

	opts.addIdentity(cmd)
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "maximum messages to return (default 50, max 500)")
	cmd.Flags().BoolVar(&opts.peek, "peek", false, "show messages without clearing them")
	cmd.Flags().BoolVar(&opts.replay, "replay", false, "show messages an earlier drain already delivered")

	return quiet(cmd)
}

func runInbox(cmd *cobra.Command, opts *inboxOptions) error {
	name, workspace, err := resolveSelf(opts.name, opts.workspace)
	if err != nil {
		return err
	}
	if opts.limit < 0 {
		return failf(exitGeneral, "--limit cannot be negative")
	}
	if opts.peek && opts.replay {
		return failf(exitGeneral, "--peek and --replay cannot be used together")
	}

	name, session, err := ensureRegistered(name, workspace)
	if err != nil {
		return err
	}

	params := protocol.InboxParams{
		Name:      name,
		Workspace: workspace,
		Session:   session,
		Limit:     opts.limit,
		Peek:      opts.peek,
		Replay:    opts.replay,
	}

	var res protocol.InboxResult
	if err := call(protocol.MethodInbox, params, &res); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if res.Messages == nil {
		res.Messages = []protocol.MessageView{}
	}
	return printJSON(out, res)
}
