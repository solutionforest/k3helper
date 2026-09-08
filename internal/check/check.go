// Package check defines the health-check engine: a registry of checks
// that each return a Result with status, evidence and remediation.
package check

import "fmt"

// Status of a check outcome.
type Status int

const (
	OK Status = iota
	Warn
	Fail
	Skip
)

func (s Status) String() string {
	switch s {
	case OK:
		return "OK"
	case Warn:
		return "WARN"
	case Fail:
		return "FAIL"
	default:
		return "SKIP"
	}
}

// Icon returns the terminal glyph for the status.
func (s Status) Icon() string {
	switch s {
	case OK:
		return "✓"
	case Warn:
		return "!"
	case Fail:
		return "✗"
	default:
		return "-"
	}
}

// Result is the outcome of one check.
type Result struct {
	ID          string   `json:"id"`
	Category    string   `json:"category"` // host | k3s | cluster | workload | network | storage
	Name        string   `json:"name"`
	Status      Status   `json:"status"`
	Summary     string   `json:"summary"`
	Details     string   `json:"details,omitempty"`
	Remediation string   `json:"remediation,omitempty"`
	Evidence    []string `json:"evidence,omitempty"` // raw command outputs backing the verdict
}

// Check is a single diagnostic. Run receives an Executor that abstracts
// "run this command somewhere" so checks are testable with fixtures.
type Check interface {
	ID() string
	Name() string
	Category() string
	// Run executes the check; it should never panic and always return a Result.
	Run(ctx Context) Result
}

// Context carries the environment a check runs against.
// Executor runs a command (locally or over SSH) and returns combined output + exit code.
type Context struct {
	Exec Executor
	// Node is the node name the check targets ("" for cluster-wide).
	Node string
}

// Executor abstracts command execution for testability.
type Executor interface {
	Run(cmd string) (stdout string, code int, err error)
}

// Runner aggregates results from a set of checks.
type Runner struct {
	checks []Check
}

// NewRunner creates a runner with the given checks.
func NewRunner(checks ...Check) *Runner {
	return &Runner{checks: checks}
}

// Add registers more checks.
func (r *Runner) Add(checks ...Check) {
	r.checks = append(r.checks, checks...)
}

// RunAll executes every check and returns results in registry order.
func (r *Runner) RunAll(ctx Context) []Result {
	out := make([]Result, 0, len(r.checks))
	for _, c := range r.checks {
		res := safeRun(c, ctx)
		out = append(out, res)
	}
	return out
}

func safeRun(c Check, ctx Context) (res Result) {
	defer func() {
		if p := recover(); p != nil {
			res = Result{
				ID: c.ID(), Category: c.Category(), Name: c.Name(),
				Status:  Fail,
				Summary: fmt.Sprintf("check panicked: %v", p),
			}
		}
	}()
	return c.Run(ctx)
}
