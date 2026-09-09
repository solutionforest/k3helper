package check

import (
	"fmt"
	"strings"
)

// Distro is the Kubernetes distribution installed on a node. The host layer
// differs between them: k3s ships one unit that embeds containerd, while
// kubeadm runs kubelet and a separate container runtime.
type Distro string

const (
	DistroK3s     Distro = "k3s"
	DistroKubeadm Distro = "kubeadm"
	DistroNone    Distro = "none"
)

// Units returns the systemd units that must be active for the node to work.
func (d Distro) Units(role string) []string {
	switch d {
	case DistroK3s:
		if role == "agent" {
			return []string{"k3s-agent"}
		}
		return []string{"k3s"}
	case DistroKubeadm:
		// kubelet runs on every node; containerd is checked separately
		// because a node can have kubelet up with a dead runtime.
		return []string{"kubelet", "containerd"}
	}
	return nil
}

// Kubeconfig returns the admin kubeconfig path for the distribution.
func (d Distro) Kubeconfig() string {
	switch d {
	case DistroK3s:
		return "/etc/rancher/k3s/k3s.yaml"
	case DistroKubeadm:
		return "/etc/kubernetes/admin.conf"
	}
	return ""
}

// UnitPresent reports whether a systemd unit file exists on the node.
//
// `systemctl is-active` prints "inactive" for a unit that was never installed,
// so presence has to be asked separately — that ambiguity is why a missing k3s
// used to be reported as "restart the service" rather than "install it".
//
// It asks the filesystem first and systemd second. `systemctl` talks to the
// system bus, and an SSH user who cannot reach that bus — a host without
// dbus, a locked-down login — gets "Failed to connect to bus" for every
// query. Reading that as "the unit does not exist" told operators k3s was not
// installed on nodes where k3s was running and serving traffic.
func UnitPresent(exec Executor, unit string) bool {
	out, _, err := exec.Run(fmt.Sprintf(
		`ls /etc/systemd/system/%[1]s.service /run/systemd/system/%[1]s.service `+
			`/lib/systemd/system/%[1]s.service /usr/lib/systemd/system/%[1]s.service 2>/dev/null | head -1`,
		unit))
	if err == nil && strings.TrimSpace(out) != "" {
		return true
	}
	// A unit can also be generated at runtime and never written to any of
	// those directories, so fall back to systemd's own view — with sudo,
	// which is the path that still works when the login's bus access does not.
	out, _, err = exec.Run(fmt.Sprintf(
		`sudo -n systemctl list-unit-files %s.service --no-legend 2>/dev/null | head -1`, unit))
	return err == nil && strings.Contains(out, unit)
}

// unitPresent is the unexported spelling the checks in this package use.
func unitPresent(exec Executor, unit string) bool { return UnitPresent(exec, unit) }

// ServiceState returns what `systemctl is-active` says about a unit.
//
// It tries unprivileged first and privileged second, and returns an empty
// state with the explanation when neither produced something systemd actually
// prints. Both halves matter: a host without passwordless sudo answers only
// the first, a host whose login cannot reach the system bus answers only the
// second, and inventing a state from an error message would report a running
// service as down.
func ServiceState(exec Executor, unit string) (state, problem string) {
	for _, cmd := range []string{
		fmt.Sprintf(`systemctl is-active %s 2>&1`, unit),
		fmt.Sprintf(`sudo -n systemctl is-active %s 2>&1`, unit),
	} {
		out, _, err := exec.Run(cmd)
		s := strings.TrimSpace(out)
		if err == nil && IsServiceState(s) {
			return s, ""
		}
		if s != "" {
			problem = s
		}
	}
	if problem == "" {
		problem = "systemctl produced no output"
	}
	return "", problem
}

// IsServiceState reports whether out is something `systemctl is-active`
// actually prints, as opposed to an error from the shell or the bus.
func IsServiceState(out string) bool {
	switch out {
	case "active", "inactive", "failed", "activating", "deactivating",
		"reloading", "unknown", "maintenance":
		return true
	}
	return false
}

// DetectDistro works out which distribution a node runs by looking for the
// unit files each one installs.
func DetectDistro(exec Executor) Distro {
	for _, unit := range []string{"k3s", "k3s-agent"} {
		if unitPresent(exec, unit) {
			return DistroK3s
		}
	}
	if unitPresent(exec, "kubelet") {
		return DistroKubeadm
	}
	return DistroNone
}

// ServiceCheck verifies the node's Kubernetes services are active, adapting
// to whichever distribution is installed.
type ServiceCheck struct {
	// Role of this node: "server" or "agent".
	Role string
	// Distro pins the distribution; empty means detect it.
	Distro Distro
}

func (c ServiceCheck) ID() string       { return "k8s.service" }
func (c ServiceCheck) Name() string     { return "Kubernetes services" }
func (c ServiceCheck) Category() string { return "k8s" }

func (c ServiceCheck) Run(ctx Context) Result {
	res := Result{ID: c.ID(), Category: c.Category(), Name: c.Name()}

	distro := c.Distro
	if distro == "" {
		distro = DetectDistro(ctx.Exec)
	}
	if distro == DistroNone {
		res.Status = Fail
		res.Summary = "no Kubernetes service found (neither k3s nor kubelet is installed)"
		res.Remediation = "Install k3s with `k3helper vm setup`, or `curl -sfL https://get.k3s.io | sh -`. For a kubeadm cluster, install kubelet and run `kubeadm join`/`kubeadm init`."
		return res
	}

	var inactive, missing []string
	var evidence []string
	for _, unit := range distro.Units(c.Role) {
		if !unitPresent(ctx.Exec, unit) {
			// containerd is optional on k3s-style installs that embed it.
			if unit == "containerd" {
				continue
			}
			missing = append(missing, unit)
			continue
		}
		state, problem := ServiceState(ctx.Exec, unit)
		if state == "" {
			// Neither query answered: say so rather than reporting a unit
			// whose state is unknown as one that is down.
			res.Status = Skip
			res.Summary = "could not read the state of " + unit
			res.Details = problem
			return res
		}
		evidence = append(evidence, unit+"="+state)
		if state != "active" {
			inactive = append(inactive, fmt.Sprintf("%s is %s", unit, state))
		}
	}
	res.Evidence = evidence

	switch {
	case len(missing) > 0:
		res.Status = Fail
		res.Summary = fmt.Sprintf("%s unit(s) not installed: %s", distro, strings.Join(missing, ", "))
		res.Remediation = fmt.Sprintf("This node reports %s but is missing %s. Re-run the install, or `k3helper vm setup` to bootstrap it.",
			distro, strings.Join(missing, ", "))
	case len(inactive) > 0:
		res.Status = Fail
		res.Summary = strings.Join(inactive, "; ")
		unit := strings.Fields(inactive[0])[0]
		res.Remediation = fmt.Sprintf("Inspect logs: `sudo journalctl -u %s -n 100 --no-pager`; then `sudo systemctl restart %s`", unit, unit)
	default:
		res.Status = OK
		res.Summary = fmt.Sprintf("%s services active (%s)", distro, strings.Join(evidence, ", "))
	}
	return res
}

// K3sServiceCheck is the k3s-only predecessor of ServiceCheck, kept so
// existing callers and fixtures keep working.
//
// Deprecated: use ServiceCheck, which also handles kubeadm nodes.
type K3sServiceCheck struct {
	Role string
}

func (c K3sServiceCheck) ID() string       { return "k3s.service" }
func (c K3sServiceCheck) Name() string     { return "k3s systemd service" }
func (c K3sServiceCheck) Category() string { return "k3s" }

func (c K3sServiceCheck) Run(ctx Context) Result {
	unit := "k3s"
	if c.Role == "agent" {
		unit = "k3s-agent"
	}
	active, problem := ServiceState(ctx.Exec, unit)
	out := active
	if active == "" {
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Skip,
			Summary: "could not read the state of " + unit, Details: problem}
	}
	switch {
	case active == "active":
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: OK,
			Summary: fmt.Sprintf("%s is active", unit), Evidence: []string{out}}
	// Only "inactive" is ambiguous: `systemctl is-active` prints it both for a
	// stopped unit and for one that was never installed. Any other state
	// ("failed", "activating", ...) proves the unit exists, so don't ask.
	case active == "inactive" && !unitPresent(ctx.Exec, unit):
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Fail,
			Summary:     fmt.Sprintf("%s unit not found: k3s may not be installed", unit),
			Remediation: "Install k3s: `curl -sfL https://get.k3s.io | sh -` or run `k3helper vm setup`", Evidence: []string{out}}
	default:
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Fail,
			Summary:     fmt.Sprintf("%s is %s", unit, active),
			Remediation: fmt.Sprintf("Inspect logs: `sudo journalctl -u %s -n 100 --no-pager`; try `sudo systemctl restart %s`", unit, unit), Evidence: []string{out}}
	}
}
