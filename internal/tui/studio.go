package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/solutionforest/k3helper/internal/deploy"
	"github.com/solutionforest/k3helper/internal/kyaml"
	"github.com/solutionforest/k3helper/internal/ssh"
)

// --- YAML studio (:gen) ------------------------------------------------------

// genMsg carries a generated manifest and what verify made of it.
type genMsg struct {
	yaml   string
	params kyaml.GenParams
	body   string
	err    error
}

// genFlow generates a manifest and verifies it in the same pass, so the pane
// never shows YAML whose validity is unknown.
//
// Verifying what we just generated is not redundant: the round-trip guarantee
// (everything gen emits passes verify) is the generator's contract, and the
// studio is where a break in it would be seen first.
func genFlow(args string) tea.Cmd {
	return func() tea.Msg {
		p, err := parseGenArgs(args)
		if err != nil {
			return genMsg{err: err}
		}
		out, err := kyaml.Generate(p)
		if err != nil {
			return genMsg{err: err}
		}
		res := kyaml.Verify([]byte(out))

		var b strings.Builder
		b.WriteString(highlightYAML(out) + "\n\n")
		if len(res.Issues) == 0 {
			b.WriteString(statusOKStyle.Render("✓ verify: valid") + "\n")
		} else {
			b.WriteString(statusFailStyle.Render(fmt.Sprintf("✗ verify: %d issue(s)", len(res.Issues))) + "\n")
			for _, iss := range res.Issues {
				b.WriteString(statusFailStyle.Render(fmt.Sprintf("  line %d: %s %s", iss.Line, iss.Field, iss.Message)) + "\n")
			}
		}
		b.WriteString(helpStyle.Render("\n  s: save to " + genFileName(p) + "   esc: back"))
		return genMsg{yaml: out, params: p, body: b.String()}
	}
}

// parseGenArgs reads `:gen <kind> <name> [image] [replicas] [port]`.
func parseGenArgs(args string) (kyaml.GenParams, error) {
	fields := strings.Fields(args)
	if len(fields) < 2 {
		return kyaml.GenParams{}, fmt.Errorf(
			"usage: :gen <kind> <name> [image] [replicas] [port] — e.g. :gen deployment web nginx:1.27 2 80")
	}
	p := kyaml.GenParams{Kind: fields[0], Name: fields[1]}
	if len(fields) > 2 {
		p.Image = fields[2]
	}
	if len(fields) > 3 {
		n, err := strconv.Atoi(fields[3])
		if err != nil {
			return kyaml.GenParams{}, fmt.Errorf("replicas %q is not a number", fields[3])
		}
		p.Replicas = n
	}
	if len(fields) > 4 {
		n, err := strconv.Atoi(fields[4])
		if err != nil {
			return kyaml.GenParams{}, fmt.Errorf("port %q is not a number", fields[4])
		}
		p.Port = n
	}
	return p, nil
}

func genFileName(p kyaml.GenParams) string {
	return fmt.Sprintf("%s-%s.yaml", strings.ToLower(p.Name), strings.ToLower(p.Kind))
}

// savedMsg reports where a generated manifest landed.
type savedMsg struct {
	path string
	err  error
}

// saveGen writes the generated manifest next to the operator, refusing to
// overwrite: the studio is a scratchpad, and silently replacing a manifest
// someone hand-edited would be the worst possible surprise from it.
func saveGen(yamlDoc string, p kyaml.GenParams) tea.Cmd {
	return func() tea.Msg {
		name := genFileName(p)
		if _, err := os.Stat(name); err == nil {
			return savedMsg{err: fmt.Errorf("%s already exists — move it aside first", name)}
		}
		if err := os.WriteFile(name, []byte(yamlDoc), 0o644); err != nil {
			return savedMsg{err: err}
		}
		abs, _ := filepath.Abs(name)
		return savedMsg{path: abs}
	}
}

// --- deploy flow (:deploy <file>) -------------------------------------------

// deployMsg carries the outcome of a dry-run or an apply.
type deployMsg struct {
	result  *deploy.Result
	applied bool // false = this was the dry-run/diff pass
	file    string
	body    string
	err     error
}

// dryRunDeploy shows what applying a manifest would change, without changing
// anything. The operator applies it with a second keystroke.
func dryRunDeploy(client *ssh.Client, path, namespace string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return deployMsg{file: path, err: fmt.Errorf("no server connection")}
		}
		if _, err := os.Stat(path); err != nil {
			return deployMsg{file: path, err: fmt.Errorf("read manifest: %w", err)}
		}
		res, err := deploy.Deploy(client, path, deploy.Options{
			Namespace: namespace, DryRun: true, Diff: true,
		})
		if err != nil {
			return deployMsg{file: path, err: err}
		}
		return deployMsg{result: res, file: path, body: renderDeployPlan(path, res)}
	}
}

// applyDeploy applies for real and waits for rollout.
func applyDeploy(client *ssh.Client, path, namespace string) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return deployMsg{file: path, applied: true, err: fmt.Errorf("no server connection")}
		}
		res, err := deploy.Deploy(client, path, deploy.Options{
			Namespace: namespace, WaitTimeout: 2 * time.Minute,
		})
		if err != nil {
			return deployMsg{file: path, applied: true, err: err}
		}
		return deployMsg{result: res, applied: true, file: path, body: renderDeployResult(path, res)}
	}
}

// renderDeployPlan is the side-by-side-in-spirit diff pane: what exists now
// versus what the manifest says, plus the resources the apply would touch.
func renderDeployPlan(path string, res *deploy.Result) string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("dry-run: "+path) + "\n\n")
	if len(res.Applied) > 0 {
		b.WriteString(sectionStyle.Render("Would apply:") + "\n")
		for _, a := range res.Applied {
			b.WriteString("  " + a + "\n")
		}
		b.WriteString("\n")
	}
	if len(res.Errors) > 0 {
		b.WriteString(statusFailStyle.Render("Errors:") + "\n")
		for _, e := range res.Errors {
			b.WriteString(statusFailStyle.Render("  "+e) + "\n")
		}
		b.WriteString("\n")
	}
	if strings.TrimSpace(res.Diff) == "" {
		b.WriteString(statusOKStyle.Render("no changes — the cluster already matches this manifest") + "\n")
	} else {
		b.WriteString(sectionStyle.Render("Diff against live state:") + "\n")
		b.WriteString(colorDiff(res.Diff) + "\n")
	}
	b.WriteString(helpStyle.Render("\n  a: apply   esc: back"))
	return b.String()
}

// renderDeployResult is the pane after a real apply.
func renderDeployResult(path string, res *deploy.Result) string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("applied: "+path) + "\n\n")
	for _, a := range res.Applied {
		b.WriteString(statusOKStyle.Render("  ✓ ") + a + "\n")
	}
	if len(res.RolledOut) > 0 {
		b.WriteString("\n" + sectionStyle.Render("Rolled out:") + "\n")
		for _, r := range res.RolledOut {
			b.WriteString(statusOKStyle.Render("  ✓ ") + r + "\n")
		}
	}
	if len(res.Errors) > 0 {
		b.WriteString("\n" + statusFailStyle.Render("Errors:") + "\n")
		for _, e := range res.Errors {
			b.WriteString(statusFailStyle.Render("  ✗ "+e) + "\n")
		}
	}
	b.WriteString(helpStyle.Render("\n  esc: back"))
	return b.String()
}

// colorDiff colours a unified diff the way every diff tool does, because an
// uncoloured diff of a large manifest is unreadable in a terminal pane.
func colorDiff(diff string) string {
	var b strings.Builder
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			b.WriteString(sectionStyle.Render(line) + "\n")
		case strings.HasPrefix(line, "+"):
			b.WriteString(statusOKStyle.Render(line) + "\n")
		case strings.HasPrefix(line, "-"):
			b.WriteString(statusFailStyle.Render(line) + "\n")
		case strings.HasPrefix(line, "@@"):
			b.WriteString(helpStyle.Render(line) + "\n")
		default:
			b.WriteString(line + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// rolloutFraction is how much of an apply reached its desired state, for the
// progress bar. Nothing applied is a full bar rather than an empty one: an
// apply of zero resources did all of the work there was.
func rolloutFraction(res *deploy.Result) float64 {
	if res == nil || len(res.Applied) == 0 {
		return 1
	}
	return float64(len(res.RolledOut)) / float64(len(res.Applied))
}
