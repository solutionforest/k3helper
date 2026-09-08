package check

import (
	"strings"
	"testing"
)

func TestDiskUsageCheck(t *testing.T) {
	c := DiskUsageCheck{}
	cases := []struct {
		name   string
		out    string
		status Status
	}{
		{"healthy 40%", "40%\n", OK},
		{"warn 91%", "91%\n", Warn},
		{"fail 97%", "97%\n", Fail},
		{"fail 100%", "100%\n", Fail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := Context{Exec: MapExec{
				`df -P / | tail -1 | awk '{print $5}'`: {Out: tc.out, Code: 0},
			}}
			res := c.Run(ctx)
			if res.Status != tc.status {
				t.Errorf("status = %s, want %s (summary: %s)", res.Status, tc.status, res.Summary)
			}
			if tc.status != OK && res.Remediation == "" {
				t.Error("non-OK result must include remediation")
			}
		})
	}
}

func TestDiskUsageCheckUnparseable(t *testing.T) {
	c := DiskUsageCheck{}
	ctx := Context{Exec: MapExec{
		`df -P / | tail -1 | awk '{print $5}'`: {Out: "garbage\n", Code: 0},
	}}
	res := c.Run(ctx)
	if res.Status != Fail {
		t.Errorf("status = %s, want FAIL", res.Status)
	}
}

func TestMemoryCheck(t *testing.T) {
	c := MemoryCheck{}
	cases := []struct {
		out    string
		status Status
	}{
		{"2048\n", OK},
		{"250\n", Warn},
		{"50\n", Fail},
	}
	for _, tc := range cases {
		ctx := Context{Exec: MapExec{
			`free -m | awk '/^Mem:/{print $7}'`: {Out: tc.out, Code: 0},
		}}
		res := c.Run(ctx)
		if res.Status != tc.status {
			t.Errorf("out=%q status = %s, want %s", tc.out, res.Status, tc.status)
		}
	}
}

func TestSwapCheck(t *testing.T) {
	c := SwapCheck{}
	off := Context{Exec: MapExec{
		`swapon --noheadings --show 2>/dev/null | wc -l`: {Out: "0\n", Code: 0},
	}}
	if res := c.Run(off); res.Status != OK {
		t.Errorf("swap off: status = %s, want OK", res.Status)
	}
	on := Context{Exec: MapExec{
		`swapon --noheadings --show 2>/dev/null | wc -l`: {Out: "1\n", Code: 0},
	}}
	res := c.Run(on)
	if res.Status != Warn {
		t.Errorf("swap on: status = %s, want WARN", res.Status)
	}
	if !strings.Contains(res.Remediation, "swapoff") {
		t.Error("swap remediation should mention swapoff")
	}
}

func TestCgroupCheck(t *testing.T) {
	c := CgroupCheck{}
	good := Context{Exec: MapExec{
		`cat /sys/fs/cgroup/cgroup.controllers 2>/dev/null || echo legacy`: {Out: "cpuset cpu io memory pids\n", Code: 0},
	}}
	if res := c.Run(good); res.Status != OK {
		t.Errorf("good cgroup: %s", res.Status)
	}
	bad := Context{Exec: MapExec{
		`cat /sys/fs/cgroup/cgroup.controllers 2>/dev/null || echo legacy`: {Out: "cpu io pids\n", Code: 0},
	}}
	res := c.Run(bad)
	if res.Status != Fail {
		t.Errorf("bad cgroup: %s, want FAIL", res.Status)
	}
	if !strings.Contains(res.Remediation, "cgroup_enable") {
		t.Error("cgroup remediation should mention cgroup_enable (RPi classic)")
	}
}

func TestK3sServiceCheck(t *testing.T) {
	c := K3sServiceCheck{Role: "server"}
	active := Context{Exec: MapExec{
		`sudo -n systemctl is-active k3s 2>/dev/null`: {Out: "active\n", Code: 0},
	}}
	if res := c.Run(active); res.Status != OK {
		t.Errorf("active: %s", res.Status)
	}
	dead := Context{Exec: MapExec{
		`sudo -n systemctl is-active k3s 2>/dev/null`: {Out: "failed\n", Code: 3},
	}}
	res := c.Run(dead)
	if res.Status != Fail {
		t.Errorf("failed: %s, want FAIL", res.Status)
	}
	if !strings.Contains(res.Remediation, "journalctl") {
		t.Error("failed service remediation should mention journalctl")
	}
	// agent role checks k3s-agent unit
	agent := K3sServiceCheck{Role: "agent"}
	agentCtx := Context{Exec: MapExec{
		`sudo -n systemctl is-active k3s-agent 2>/dev/null`: {Out: "active\n", Code: 0},
	}}
	if res := agent.Run(agentCtx); res.Status != OK {
		t.Errorf("agent active: %s", res.Status)
	}
}

func TestRunnerAggregatesAndNeverPanics(t *testing.T) {
	panicky := panickyCheck{}
	r := NewRunner(DiskUsageCheck{}, panicky)
	res := r.RunAll(Context{Exec: MapExec{
		`df -P / | tail -1 | awk '{print $5}'`: {Out: "50%\n", Code: 0},
	}})
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2", len(res))
	}
	if res[0].Status != OK {
		t.Errorf("disk check = %s, want OK", res[0].Status)
	}
	if res[1].Status != Fail || res[1].Summary == "" {
		t.Errorf("panic should be converted to FAIL result, got %+v", res[1])
	}
}

type panickyCheck struct{}

func (panickyCheck) ID() string       { return "test.panic" }
func (panickyCheck) Name() string     { return "Panics" }
func (panickyCheck) Category() string { return "test" }
func (panickyCheck) Run(ctx Context) Result {
	panic("boom")
}
