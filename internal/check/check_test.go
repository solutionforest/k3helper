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
		`systemctl is-active k3s 2>&1`: {Out: "active\n", Code: 0},
	}}
	if res := c.Run(active); res.Status != OK {
		t.Errorf("active: %s", res.Status)
	}
	dead := Context{Exec: MapExec{
		`systemctl is-active k3s 2>&1`: {Out: "failed\n", Code: 3},
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
		`systemctl is-active k3s-agent 2>&1`: {Out: "active\n", Code: 0},
	}}
	if res := agent.Run(agentCtx); res.Status != OK {
		t.Errorf("agent active: %s", res.Status)
	}
}

// Neither systemctl query works on every host: a host without passwordless
// sudo answers only the unprivileged one, and a login that cannot reach the
// systemd bus answers only the privileged one. Reading either failure as a
// state is how a running service gets reported as down.
func TestServiceStateTriesBothPrivilegeLevels(t *testing.T) {
	noBus := MapExec{
		`systemctl is-active k3s 2>&1`:         {Out: "Failed to connect to bus: No such file or directory\n", Code: 1},
		`sudo -n systemctl is-active k3s 2>&1`: {Out: "active\n", Code: 0},
	}
	if state, _ := ServiceState(noBus, "k3s"); state != "active" {
		t.Errorf("no-bus host: state = %q, want active", state)
	}

	noSudo := MapExec{
		`systemctl is-active k3s 2>&1`:         {Out: "inactive\n", Code: 3},
		`sudo -n systemctl is-active k3s 2>&1`: {Out: "sudo: a password is required\n", Code: 1},
	}
	if state, _ := ServiceState(noSudo, "k3s"); state != "inactive" {
		t.Errorf("no-sudo host: state = %q, want inactive", state)
	}

	// Neither answered: report no state and say why, rather than inventing one.
	blind := MapExec{
		`systemctl is-active k3s 2>&1`:         {Out: "Failed to connect to bus\n", Code: 1},
		`sudo -n systemctl is-active k3s 2>&1`: {Out: "sudo: a password is required\n", Code: 1},
	}
	state, problem := ServiceState(blind, "k3s")
	if state != "" {
		t.Errorf("unreadable host: state = %q, want empty", state)
	}
	if !strings.Contains(problem, "sudo") {
		t.Errorf("problem should carry the reason, got %q", problem)
	}
}

// A node whose login cannot query systemd used to report "k3s is not
// installed" while k3s was running: unit presence is a fact about the
// filesystem, so read it there first.
func TestUnitPresentReadsTheFilesystemFirst(t *testing.T) {
	lsCmd := `ls /etc/systemd/system/k3s.service /run/systemd/system/k3s.service ` +
		`/lib/systemd/system/k3s.service /usr/lib/systemd/system/k3s.service 2>/dev/null | head -1`
	onDisk := MapExec{lsCmd: {Out: "/etc/systemd/system/k3s.service\n", Code: 0}}
	if !UnitPresent(onDisk, "k3s") {
		t.Error("a unit file on disk must count as present even when systemctl cannot be reached")
	}

	// Nothing on disk and nothing from systemd: absent.
	if UnitPresent(MapExec{}, "k3s") {
		t.Error("no unit file and no systemd answer must read as absent")
	}

	// Generated units exist only in systemd's view.
	generated := MapExec{
		`sudo -n systemctl list-unit-files k3s.service --no-legend 2>/dev/null | head -1`: {
			Out: "k3s.service enabled\n", Code: 0},
	}
	if !UnitPresent(generated, "k3s") {
		t.Error("a unit systemd knows about but that has no file must still count as present")
	}
}

// The distribution is detected from unit presence, so it inherits the same
// failure: this is the assertion that a running k3s is never reported as "no
// Kubernetes service found".
func TestDetectDistroWithoutSystemctlAccess(t *testing.T) {
	agentOnDisk := MapExec{
		`ls /etc/systemd/system/k3s-agent.service /run/systemd/system/k3s-agent.service ` +
			`/lib/systemd/system/k3s-agent.service /usr/lib/systemd/system/k3s-agent.service 2>/dev/null | head -1`: {
			Out: "/etc/systemd/system/k3s-agent.service\n", Code: 0},
	}
	if got := DetectDistro(agentOnDisk); got != DistroK3s {
		t.Errorf("DetectDistro = %q, want k3s", got)
	}
	if got := DetectDistro(MapExec{}); got != DistroNone {
		t.Errorf("DetectDistro with nothing installed = %q, want none", got)
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
