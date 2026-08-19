package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/praneethravuri/tether/internal/protocol"
)

const releaseLong = `Release a claim you hold.

--if-claim-id must be the lease id ` + "`tether claim`" + ` returned. The check happens
in one atomic step on the daemon: a stale or mismatched id is rejected
outright, never guessed at or silently ignored -- it can never release a
claim it did not itself acquire, even one on the same key.`

type releaseOptions struct {
	identityFlags
	leaseID string
}

func newReleaseCmd() *cobra.Command {
	var opts releaseOptions

	cmd := &cobra.Command{
		Use:     "release <key>",
		Short:   "Release a claim you hold",
		Long:    releaseLong,
		Example: "  tether release src/main.go --if-claim-id 1a2b3c4d5e6f7890a1b2c3d4e5f67890",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRelease(cmd, args[0], &opts)
		},
	}

	opts.addWorkspace(cmd)
	cmd.Flags().StringVar(&opts.leaseID, "if-claim-id", "",
		"lease id returned by tether claim (required)")

	return quiet(cmd)
}

func runRelease(cmd *cobra.Command, key string, opts *releaseOptions) error {
	ws, err := resolveWorkspace(opts.workspace)
	if err != nil {
		return err
	}

	leaseID := strings.TrimSpace(opts.leaseID)
	if leaseID == "" {
		return failf(exitGeneral,
			"--if-claim-id is required — pass the lease id `tether claim` returned")
	}

	params := protocol.ReleaseParams{Workspace: ws, Key: key, LeaseID: leaseID}
	var res protocol.ReleaseResult
	if err := call(protocol.MethodRelease, params, &res); err != nil {
		return releaseError(key, ws, err)
	}

	out := cmd.OutOrStdout()
	return printJSON(out, res)
}

// releaseError maps a daemon failure to an actionable message and exit code:
// a mismatched lease id shares register's conflict code, a missing claim
// falls through to the general failure path.
func releaseError(key, workspace string, err error) error {
	code, ok := daemonCode(err)
	if !ok {
		return err
	}
	switch code {
	case protocol.CodeConflict:
		return fail(exitConflict, fmt.Errorf(
			"cannot release %s in %s: that claim id does not match its current lease "+
				"(it may have been renewed or reclaimed since you got it) — "+
				"run `tether claims` to see who holds it now", key, workspace))
	case protocol.CodeNotFound:
		return fail(exitGeneral, fmt.Errorf("cannot release %s in %s: no such claim", key, workspace))
	default:
		return err
	}
}
