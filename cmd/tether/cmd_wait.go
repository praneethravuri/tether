package main

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/praneethravuri/tether/internal/protocol"
)

// defaultWaitTimeout is long enough to be useful in a shell loop and short
// enough that a forgotten `tether wait` eventually returns.
const defaultWaitTimeout = 60 * time.Second

// maxWaitTimeout keeps a typo like --timeout 60m0s0h from parking a process
// for a day.
const maxWaitTimeout = 24 * time.Hour

const waitLong = `Block until mail is waiting for this agent.

The timeout is a Go duration such as 30s, 5m or 1h30m. Exits 0 as soon as there
is something to read, and 4 if the timeout expires first, so a shell can branch
on it:

  if tether wait --timeout 2m; then tether inbox; fi

This is the polling-free way to idle: agents whose harness the daemon cannot
wake should sit in wait rather than calling inbox in a loop.

Output is JSON by default.`

type waitOptions struct {
	identityFlags
	timeout time.Duration
}

func newWaitCmd() *cobra.Command {
	var opts waitOptions

	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Block until a message is waiting",
		Long:  waitLong,
		Example: "  tether wait\n" +
			"  tether wait --timeout 5m\n" +
			"  tether wait --as frontend --timeout 30s",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWait(cmd, &opts)
		},
	}

	opts.addIdentity(cmd)
	cmd.Flags().DurationVar(&opts.timeout, "timeout", defaultWaitTimeout,
		"how long to block, as a Go duration (30s, 5m, 1h30m)")

	return quiet(cmd)
}

func runWait(cmd *cobra.Command, opts *waitOptions) error {
	if opts.timeout <= 0 {
		return failf(exitGeneral, "--timeout must be greater than zero, got %s", opts.timeout)
	}
	if opts.timeout > maxWaitTimeout {
		return failf(exitGeneral, "--timeout must not exceed %s, got %s",
			maxWaitTimeout, opts.timeout)
	}

	name, workspace, err := resolveSelf(opts.name, opts.workspace)
	if err != nil {
		return err
	}

	name, session, err := ensureRegistered(name, workspace)
	if err != nil {
		return err
	}

	res, err := waitUpTo(name, workspace, session, opts.timeout)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()

	if err := printJSON(out, res); err != nil {
		return err
	}

	if res.TimedOut && res.Pending == 0 {
		return silentExit(exitTimeout)
	}
	return nil
}

// waitRetryBaseDelay/waitRetryMaxDelay bound the backoff between
// transport-failure retries, so a daemon that never comes back is polled a
// few times a second rather than in a tight loop.
const (
	waitRetryBaseDelay = 200 * time.Millisecond
	waitRetryMaxDelay  = 5 * time.Second
)

// waitUpTo blocks for up to total, transparently re-issuing wait past the
// daemon's internal per-request ceiling (WaitResult.Capped) so the CLI's
// contract is genuinely "blocks up to --timeout".
func waitUpTo(name, workspace, session string, total time.Duration) (protocol.WaitResult, error) {
	deadline := time.Now().Add(total)
	remaining := total
	autoStart := true // a daemon that crashes on every start must not be re-spawned every retry
	transportFailures := 0

	for {
		timeoutMS := int(remaining.Milliseconds())
		if timeoutMS < 1 {
			timeoutMS = 1
		}
		params := protocol.WaitParams{
			Name:      name,
			Workspace: workspace,
			Session:   session,
			TimeoutMS: timeoutMS,
		}

		var res protocol.WaitResult
		err := doCall(protocol.MethodWait, params, &res, waitCallTimeout(remaining), autoStart)
		if err != nil {
			// A transport failure (the daemon was killed while this call was
			// parked, say) is worth reconnecting for. A daemon-side error (bad
			// request, conflict) means something is genuinely wrong, so that
			// still returns immediately.
			if _, fromDaemon := daemonCode(err); fromDaemon {
				return protocol.WaitResult{}, err
			}
			autoStart = false
			transportFailures++

			remaining = time.Until(deadline)
			if remaining <= 0 {
				return protocol.WaitResult{}, err
			}
			delay := backoff(transportFailures)
			if delay > remaining {
				delay = remaining
			}
			time.Sleep(delay)

			remaining = time.Until(deadline)
			if remaining <= 0 {
				return protocol.WaitResult{}, err
			}
			continue
		}

		if !res.TimedOut || res.Pending > 0 {
			return res, nil
		}
		if !res.Capped {
			return res, nil // genuine timeout
		}

		remaining = time.Until(deadline)
		if remaining <= 0 {
			return protocol.WaitResult{Pending: 0, TimedOut: true}, nil // Capped is an internal detail by now
		}
	}
}

// backoff doubles from waitRetryBaseDelay per consecutive failure, capped at
// waitRetryMaxDelay.
func backoff(failures int) time.Duration {
	d := waitRetryBaseDelay << uint(failures-1)
	if d > waitRetryMaxDelay || d <= 0 { // <= 0 catches the shift overflowing on a very long run
		return waitRetryMaxDelay
	}
	return d
}
