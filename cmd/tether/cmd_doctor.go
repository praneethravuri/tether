package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/praneethravuri/tether/internal/protocol"
)

const doctorLong = `Check that tether is actually working, and say so plainly when it is not.

doctor reports the socket it would talk to, whether the daemon is answering,
which workspace this directory resolves to, which harness it detects, and
every agent registered here.

Nothing currently pushes a notification when mail arrives, so every agent
sees a message only if it polls with ` + "`tether inbox`" + ` or blocks on
` + "`tether wait`" + `.

Output is JSON by default. Exits 3 when no daemon is reachable.`

// doctorReport is the doctor command result.
type doctorReport struct {
	DaemonRunning bool                 `json:"daemon_running"`
	Socket        string               `json:"socket"`
	Workspace     string               `json:"workspace"`
	Cwd           string               `json:"cwd"`
	Harness       string               `json:"harness"`
	SessionID     string               `json:"session_id,omitempty"`
	DBPath        string               `json:"db_path,omitempty"`
	DBSizeBytes   int64                `json:"db_size_bytes,omitempty"`
	Agents        []protocol.AgentView `json:"agents"`
	DaemonLogPath string               `json:"daemon_log_path,omitempty"`
	Warnings      []string             `json:"warnings"`
	Error         string               `json:"error,omitempty"`
}

func newDoctorCmd() *cobra.Command {
	var opts identityFlags

	cmd := &cobra.Command{
		Use:     "doctor",
		Short:   "Check the daemon, this workspace, and every agent here",
		Long:    doctorLong,
		Example: "  tether doctor",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd, &opts)
		},
	}

	opts.addWorkspace(cmd)
	return quiet(cmd)
}

func runDoctor(cmd *cobra.Command, opts *identityFlags) error {
	report := collectDoctorReport(opts.workspace)

	out := cmd.OutOrStdout()
	if err := printJSON(out, report); err != nil {
		return err
	}

	if !report.DaemonRunning {
		return silentExit(exitNoDaemon)
	}
	return nil
}

// collectDoctorReport never fails; anything undeterminable is recorded in the report.
func collectDoctorReport(workspaceFlag string) doctorReport {
	report := doctorReport{
		Agents:   []protocol.AgentView{},
		Warnings: []string{},
	}

	socketResolved := false
	if sock, err := protocol.SocketPath(); err == nil {
		report.Socket = sock
		socketResolved = true
	} else {
		report.Socket = "unknown"
		report.Error = fmt.Sprintf("cannot work out where the tether socket lives: %v", err)
	}

	if cwd, err := os.Getwd(); err == nil {
		report.Cwd = cwd
	}

	if ws, err := resolveWorkspace(workspaceFlag); err == nil {
		report.Workspace = ws
	} else {
		report.Workspace = "unknown"
		report.Warnings = append(report.Warnings,
			fmt.Sprintf("WARNING: cannot resolve the workspace for this directory: %v", err))
	}

	report.Harness, report.SessionID = detectHarness()
	if report.Harness == harnessUnknown {
		report.Warnings = append(report.Warnings,
			"WARNING: this harness was not recognised, so agents registered from here "+
				"show harness \"unknown\".")
	}

	if dbPath, err := protocol.DBPath(); err == nil {
		report.DBPath = dbPath
		if info, err := os.Stat(dbPath); err == nil {
			report.DBSizeBytes = info.Size()
		}
	}
	if logPath, err := protocol.LogPath(); err == nil {
		report.DaemonLogPath = logPath
	}
	if !socketResolved {
		return report
	}

	var who protocol.LsResult
	err := doCall(protocol.MethodLs, protocol.LsParams{Workspace: report.Workspace}, &who,
		defaultCallTimeout, false)
	switch err {
	case nil:
		report.DaemonRunning = true
		if who.Agents != nil {
			report.Agents = who.Agents
		}
	default:
		var ee *exitError
		if errors.As(err, &ee) && ee.code == exitNoDaemon {
			report.DaemonRunning = false
		} else {
			report.DaemonRunning = true // socket answered but didn't like the question
		}
		report.Error = err.Error()
	}

	return report
}
