package check

import (
	"fmt"
	"strconv"
	"strings"
)

// --- host-level checks (run ON a node via Executor) ---

// DiskUsageCheck warns when root filesystem usage exceeds threshold.
type DiskUsageCheck struct{ ThresholdPercent int }

func (c DiskUsageCheck) ID() string       { return "host.disk" }
func (c DiskUsageCheck) Name() string     { return "Root filesystem usage" }
func (c DiskUsageCheck) Category() string { return "host" }

func (c DiskUsageCheck) Run(ctx Context) Result {
	out, code, err := ctx.Exec.Run(`df -P / | tail -1 | awk '{print $5}'`)
	if err != nil || code != 0 {
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Fail,
			Summary: "could not read disk usage", Details: out}
	}
	pctStr := strings.TrimSuffix(strings.TrimSpace(out), "%")
	pct, err := strconv.Atoi(pctStr)
	if err != nil {
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Fail,
			Summary: "unparseable df output", Details: out}
	}
	th := c.ThresholdPercent
	if th <= 0 {
		th = 90
	}
	if pct >= 95 {
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Fail,
			Summary:     fmt.Sprintf("disk %d%% full (>=95%%): k3s will report DiskPressure and evict pods", pct),
			Remediation: "Free space: `docker system prune`/image GC on nodes, enlarge volume, or clean /var/log and /var/lib/rancher/k3s/agent/containerd", Evidence: []string{out}}
	}
	if pct >= th {
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Warn,
			Summary:     fmt.Sprintf("disk %d%% full (threshold %d%%)", pct, th),
			Remediation: "Clean up disk before it hits DiskPressure", Evidence: []string{out}}
	}
	return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: OK,
		Summary: fmt.Sprintf("disk %d%% used", pct), Evidence: []string{out}}
}

// MemoryCheck warns when available memory is critically low.
type MemoryCheck struct{}

func (c MemoryCheck) ID() string       { return "host.memory" }
func (c MemoryCheck) Name() string     { return "Available memory" }
func (c MemoryCheck) Category() string { return "host" }

func (c MemoryCheck) Run(ctx Context) Result {
	out, code, err := ctx.Exec.Run(`free -m | awk '/^Mem:/{print $7}'`)
	if err != nil || code != 0 {
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Fail,
			Summary: "could not read memory info", Details: out}
	}
	availMB, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Fail,
			Summary: "unparseable free output", Details: out}
	}
	switch {
	case availMB < 100:
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Fail,
			Summary:     fmt.Sprintf("only %dMB memory available: pods will be OOMKilled, node under MemoryPressure", availMB),
			Remediation: "Add memory to the node, or reduce workloads; check for runaway processes", Evidence: []string{out}}
	case availMB < 300:
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Warn,
			Summary:     fmt.Sprintf("low memory: %dMB available", availMB),
			Remediation: "Consider adding memory or setting resource limits on workloads", Evidence: []string{out}}
	default:
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: OK,
			Summary: fmt.Sprintf("%dMB memory available", availMB), Evidence: []string{out}}
	}
}

// SwapCheck fails when swap is enabled (k3s/kubelet requires swap off).
type SwapCheck struct{}

func (c SwapCheck) ID() string       { return "host.swap" }
func (c SwapCheck) Name() string     { return "Swap disabled" }
func (c SwapCheck) Category() string { return "host" }

func (c SwapCheck) Run(ctx Context) Result {
	out, code, err := ctx.Exec.Run(`swapon --noheadings --show 2>/dev/null | wc -l`)
	if err != nil || code != 0 {
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Skip,
			Summary: "swapon not available (container?)", Details: out}
	}
	if strings.TrimSpace(out) != "0" {
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Warn,
			Summary:     "swap is enabled; kubelet may misbehave (k3s tolerates but warns)",
			Remediation: "Disable swap: `sudo swapoff -a` and remove swap entry from /etc/fstab", Evidence: []string{out}}
	}
	return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: OK,
		Summary: "swap disabled", Evidence: []string{out}}
}

// CgroupCheck verifies cgroup memory + cpuset controllers are enabled (classic RPi failure).
type CgroupCheck struct{}

func (c CgroupCheck) ID() string       { return "host.cgroup" }
func (c CgroupCheck) Name() string     { return "Cgroup controllers" }
func (c CgroupCheck) Category() string { return "host" }

func (c CgroupCheck) Run(ctx Context) Result {
	out, code, err := ctx.Exec.Run(`cat /sys/fs/cgroup/cgroup.controllers 2>/dev/null || echo legacy`)
	if err != nil || code != 0 {
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Skip,
			Summary: "cannot read cgroup info", Details: out}
	}
	if strings.Contains(out, "legacy") {
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: OK,
			Summary: "cgroup v1 (legacy) assumed present", Evidence: []string{out}}
	}
	if strings.Contains(out, "memory") && strings.Contains(out, "cpuset") {
		return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: OK,
			Summary: "cgroup controllers present", Evidence: []string{out}}
	}
	// cgroup v2 file exists but memory/cpuset missing
	return Result{ID: c.ID(), Category: c.Category(), Name: c.Name(), Status: Fail,
		Summary:     "cgroup memory/cpuset controllers missing: kubelet cannot enforce resources",
		Remediation: "Add `cgroup_enable=cpuset cgroup_memory=1 cgroup_enable=memory` to kernel cmdline (/boot/cmdline.txt or /etc/default/grub), then reboot", Evidence: []string{out}}
}

// --- fixture-based Executor for tests ---

// MapExec is an Executor returning canned outputs per command (tests).
type MapExec map[string]struct {
	Out  string
	Code int
}

func (m MapExec) Run(cmd string) (string, int, error) {
	if v, ok := m[cmd]; ok {
		return v.Out, v.Code, nil
	}
	return "", 1, nil
}
