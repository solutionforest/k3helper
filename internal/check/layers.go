package check

// This file decides which checks can run against which kind of cluster.
//
// Every check in this package reads the machine: disks, memory, swap, cgroups,
// systemd units, the registry configuration files. A cluster reached through a
// kubeconfig has no machine to read — the API server answers, and the nodes
// behind it belong to a cloud provider. Rather than let those checks run and
// fail, they are reported as skipped with the reason attached.

// HostReason explains why the host layer was not checked. It is a Skip, never
// a Fail: a managed cluster is not unhealthy for declining to give out SSH.
const HostReason = "host layer not available on a kubeconfig cluster (no SSH to the nodes)"

// HostChecks are the checks that need a machine to run on. This is the single
// list; `check` and the TUI both build their runner from it, so a check added
// in one place cannot go missing from the other.
func HostChecks(role string) []Check {
	return []Check{
		DiskUsageCheck{},
		MemoryCheck{},
		SwapCheck{},
		CgroupCheck{},
		ServiceCheck{Role: role},
		RegistryCheck{},
	}
}

// SkippedHostResults returns one Skip result per host check, so that a
// kubeconfig cluster reports the same rows as an SSH one with an honest
// verdict in them. Printing nothing at all would read as "all clear".
func SkippedHostResults(role string) []Result {
	checks := HostChecks(role)
	out := make([]Result, 0, len(checks))
	for _, c := range checks {
		out = append(out, Result{
			ID:       c.ID(),
			Category: c.Category(),
			Name:     c.Name(),
			Status:   Skip,
			Summary:  HostReason,
		})
	}
	return out
}
