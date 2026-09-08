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

// unitPresent reports whether a systemd unit file exists on the node.
// `systemctl is-active` prints "inactive" for a unit that was never
// installed, so presence has to be asked separately — that ambiguity is why
// a missing k3s used to be reported as "restart the service" rather than
// "install it".
func unitPresent(exec Executor, unit string) bool {
	out, _, err := exec.Run(fmt.Sprintf(
		`systemctl list-unit-files %s.service --no-legend 2>/dev/null | head -1`, unit))
	return err == nil && strings.Contains(out, unit)
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
		out, code, err := ctx.Exec.Run(fmt.Sprintf(`sudo -n systemctl is-active %s 2>/dev/null`, unit))
		if err != nil && code == -1 {
			res.Status = Skip
			res.Summary = "systemctl unavailable"
			res.Details = out
			return res
		}
		state := strings.TrimSpace(out)
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
	out, code, err := ctx.Exec.Run(fmt.Sprintf(`sudo -n systemctl is-active %s 2>/dev/null`, unit))
	active := strings.TrimSpace(out)
	if err != nil && code == -1 {
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Skip,
			Summary: "systemctl unavailable", Details: out}
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
