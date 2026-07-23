package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
)

// runDoctor implements `maestro doctor`: an explicit runtime preflight that
// verifies every runtime referenced by the agents config (binary on PATH,
// version probe, best-effort auth) plus the tmux dependency, and reports
// early, actionable diagnostics — the same failures that would otherwise
// surface only at pane launch (launcher.go LookPath) or as a pane
// terminal-error after the first dispatch.
//
// Verdict policy mirrors the up-time preflight: binary absence is the only
// hard failure (non-zero exit); a failed version probe or an undetectable
// credential store is reported as a warning because auth state is not
// reliably inspectable (keychain-backed logins in particular) and must not
// make doctor cry wolf on a working install.
func runDoctor(args []string) error {
	cmd := NewCommand("maestro doctor", "maestro doctor [--json] [--probe-timeout-sec <n>]")
	var jsonOutput bool
	var probeTimeoutSec int
	cmd.BoolVar(&jsonOutput, "json", false, "Output results in JSON format")
	cmd.IntVar(&probeTimeoutSec, "probe-timeout-sec", 0,
		fmt.Sprintf("Per-runtime version probe timeout in seconds (default %d)",
			int(agent.DefaultRuntimeProbeTimeout/time.Second)))
	if err := cmd.Parse(args); err != nil {
		return err
	}
	if probeTimeoutSec < 0 {
		return cmd.UsageErrorf("--probe-timeout-sec must be >= 0")
	}

	maestroDir, err := requireMaestroDir("doctor")
	if err != nil {
		return err
	}
	cfg, err := model.LoadConfig(maestroDir)
	if err != nil {
		return fmt.Errorf("maestro doctor: load config: %w", err)
	}

	rp := agent.NewRuntimePreflight()
	if probeTimeoutSec > 0 {
		rp.Timeout = time.Duration(probeTimeoutSec) * time.Second
	}
	return runDoctorReport(cfg, rp, os.Stdout, jsonOutput)
}

// doctorBinaryCheck reports the resolution of a plain binary dependency
// (currently only tmux).
type doctorBinaryCheck struct {
	Name   string                   `json:"name"`
	Path   string                   `json:"path,omitempty"`
	Error  string                   `json:"error,omitempty"`
	Status agent.RuntimeCheckStatus `json:"status"`
}

// doctorReport is the JSON payload for `maestro doctor --json`.
type doctorReport struct {
	Tmux     doctorBinaryCheck          `json:"tmux"`
	Runtimes []agent.RuntimeCheckResult `json:"runtimes"`
	OK       bool                       `json:"ok"`
}

// runDoctorReport runs all checks with the given preflight and renders the
// report to out. Split from runDoctor so tests can inject a fake
// RuntimePreflight (no PATH / env / subprocess dependence) and a buffer.
func runDoctorReport(cfg model.Config, rp *agent.RuntimePreflight, out io.Writer, jsonOutput bool) error {
	report := buildDoctorReport(context.Background(), cfg, rp)

	if jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("maestro doctor: encode report: %w", err)
		}
	} else {
		renderDoctorReport(out, report)
	}

	if !report.OK {
		// Human output already carries the diagnostics; keep stderr quiet
		// and just signal failure through the exit code.
		return &CLIError{Code: 1, Silent: true, Msg: "maestro doctor: checks failed"}
	}
	return nil
}

// buildDoctorReport executes the tmux + per-runtime checks.
func buildDoctorReport(ctx context.Context, cfg model.Config, rp *agent.RuntimePreflight) doctorReport {
	report := doctorReport{OK: true}

	report.Tmux = doctorBinaryCheck{Name: "tmux", Status: agent.RuntimeCheckOK}
	if path, err := rp.LookPath("tmux"); err != nil {
		report.Tmux.Error = fmt.Sprintf("tmux binary not found on PATH: %v", err)
		report.Tmux.Status = agent.RuntimeCheckFail
		report.OK = false
	} else {
		report.Tmux.Path = path
	}

	for _, rt := range agent.ConfiguredRuntimes(cfg) {
		res := rp.Check(ctx, rt.Name, rt.Agents)
		if res.Status == agent.RuntimeCheckFail {
			report.OK = false
		}
		report.Runtimes = append(report.Runtimes, res)
	}
	return report
}

// renderDoctorReport prints the human-readable report.
func renderDoctorReport(out io.Writer, report doctorReport) {
	_, _ = fmt.Fprintln(out, "maestro doctor — runtime preflight")
	_, _ = fmt.Fprintln(out)

	if report.Tmux.Status == agent.RuntimeCheckOK {
		_, _ = fmt.Fprintf(out, "tmux: ok (%s)\n", report.Tmux.Path)
	} else {
		_, _ = fmt.Fprintf(out, "tmux: FAIL — %s\n", report.Tmux.Error)
	}

	problems := 0
	warnings := 0
	for _, res := range report.Runtimes {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintf(out, "runtime %s (agents: %s)\n", res.Runtime, strings.Join(res.Agents, ", "))
		switch {
		case res.BinaryError != "":
			problems++
			_, _ = fmt.Fprintf(out, "  binary : FAIL — %s\n", res.BinaryError)
			_, _ = fmt.Fprintln(out, "  version: skipped (binary not found)")
			_, _ = fmt.Fprintln(out, "  auth   : skipped (binary not found)")
			continue
		default:
			_, _ = fmt.Fprintf(out, "  binary : ok   %s\n", res.BinaryPath)
		}
		if res.VersionError != "" {
			warnings++
			_, _ = fmt.Fprintf(out, "  version: warn — %s\n", res.VersionError)
		} else {
			_, _ = fmt.Fprintf(out, "  version: ok   %s\n", res.Version)
		}
		if res.AuthStatus == agent.RuntimeAuthOK {
			_, _ = fmt.Fprintf(out, "  auth   : ok   %s\n", res.AuthDetail)
		} else {
			warnings++
			_, _ = fmt.Fprintf(out, "  auth   : unknown — %s\n", res.AuthDetail)
		}
	}
	if report.Tmux.Status != agent.RuntimeCheckOK {
		problems++
	}

	_, _ = fmt.Fprintln(out)
	switch {
	case problems > 0:
		_, _ = fmt.Fprintf(out, "%d problem(s), %d warning(s). Install the missing binaries or fix the agents.* model settings in .maestro/config.yaml, then re-run `maestro doctor`.\n",
			problems, warnings)
	case warnings > 0:
		_, _ = fmt.Fprintf(out, "No hard failures, %d warning(s). Warnings are best-effort signals (auth stores are not always inspectable); the formation can still start.\n",
			warnings)
	default:
		_, _ = fmt.Fprintln(out, "All checks passed.")
	}
}
