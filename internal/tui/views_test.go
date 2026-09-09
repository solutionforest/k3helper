package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/solutionforest/k3helper/internal/check"
	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/kube"
	"github.com/solutionforest/k3helper/internal/troubleshoot"
)

// sampleWorkloads is ordered the way ListWorkloads returns them: by
// namespace, then name.
func sampleWorkloads() []kube.Workload {
	return []kube.Workload{
		{Namespace: "prod", Name: "api", Kind: "deployment", Ready: "0/2", UpToDate: 2, Available: 0,
			Age: time.Minute, Selector: "app=api", Images: "api:v2"},
		{Namespace: "prod", Name: "web", Kind: "deployment", Ready: "3/3", UpToDate: 3, Available: 3,
			Age: time.Hour, Selector: "app=web", Images: "nginx:1.27"},
	}
}

func TestResolveViewNewAliases(t *testing.T) {
	cases := map[string]view{
		"dp": viewDeployments, "deployments": viewDeployments, "deploy": viewDeployments,
		"sts": viewStatefulSets, "statefulset": viewStatefulSets,
		"ds": viewDaemonSets, "daemonsets": viewDaemonSets,
		"svc": viewServices, "services": viewServices,
		"ing": viewIngresses, "ingress": viewIngresses,
		"doctor": viewDoctor, "dr": viewDoctor,
		"xray": viewXray, "vm": viewVM, "ctx": viewCtx, "contexts": viewCtx,
	}
	for in, want := range cases {
		got, ok := resolveView(in)
		if !ok || got != want {
			t.Errorf("resolveView(%q) = %v,%v; want %v,true", in, got, ok, want)
		}
	}
}

// Every table view must have columns, or the browser renders a table with no
// header and rows that silently disappear.
func TestEveryTableViewHasColumns(t *testing.T) {
	views := []view{viewPods, viewNodes, viewEvents, viewPorts, viewDeployments, viewStatefulSets,
		viewDaemonSets, viewServices, viewIngresses, viewDoctor, viewVM, viewCtx}
	for _, v := range views {
		if len(columnsFor(v, 120, false)) == 0 {
			t.Errorf("view %q has no columns", v)
		}
	}
}

// Wide mode adds columns; a row builder that does not add the matching cells
// would shift every value one column left.
func TestWideRowsMatchWideColumns(t *testing.T) {
	pods := samplePods()
	pods[0].IP = "10.42.0.5"
	pods[0].Labels = map[string]string{"app": "web"}
	nodes := []kube.Node{{Name: "n1", Status: "Ready", Roles: "control-plane", Version: "v1.31.2",
		InternalIP: "192.168.1.5", OSImage: "Ubuntu 24.04", Kernel: "6.8.0"}}

	for _, wide := range []bool{false, true} {
		o := newRowOpts("", wide, false)
		if got, want := len(podRows(pods, o)[0]), len(columnsFor(viewPods, 200, wide)); got != want {
			t.Errorf("pods wide=%v: %d cells, %d columns", wide, got, want)
		}
		if got, want := len(nodeRows(nodes, o)[0]), len(columnsFor(viewNodes, 200, wide)); got != want {
			t.Errorf("nodes wide=%v: %d cells, %d columns", wide, got, want)
		}
		if got, want := len(workloadRows(sampleWorkloads(), o)[0]), len(columnsFor(viewDeployments, 200, wide)); got != want {
			t.Errorf("workloads wide=%v: %d cells, %d columns", wide, got, want)
		}
		svcs := []kube.Service{{Namespace: "p", Name: "web", Type: "ClusterIP", ClusterIP: "10.43.0.1",
			ExternalIP: "<none>", Ports: "80/TCP", Selector: "app=web"}}
		if got, want := len(serviceRows(svcs, o)[0]), len(columnsFor(viewServices, 200, wide)); got != want {
			t.Errorf("services wide=%v: %d cells, %d columns", wide, got, want)
		}
	}
}

// ctrl-z hides everything healthy — the whole point is that what remains is
// what needs attention.
func TestFaultsOnlyKeepsOnlyProblems(t *testing.T) {
	o := newRowOpts("", false, true)
	rows := podRows(samplePods(), o)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want only the CrashLoopBackOff pod", len(rows))
	}
	if rows[0][1] != "api-1" {
		t.Errorf("kept %q, want api-1", rows[0][1])
	}
	if got := len(workloadRows(sampleWorkloads(), o)); got != 1 {
		t.Errorf("workloads: %d rows, want only the 0/2 deployment", got)
	}
	nodes := []kube.Node{{Name: "n1", Status: "Ready"}, {Name: "n2", Status: "NotReady"}}
	if rows := nodeRows(nodes, o); len(rows) != 1 || rows[0][0] != "n2" {
		t.Errorf("nodes: %+v, want only n2", rows)
	}
}

func TestFilterIsRegexWithSubstringFallback(t *testing.T) {
	o := newRowOpts("web|api", false, false)
	if got := len(podRows(samplePods(), o)); got != 2 {
		t.Errorf("regex alternation matched %d pods, want 2", got)
	}
	// A half-typed regex must not blank the table mid-keystroke.
	bad := newRowOpts("web-(", false, false)
	if bad.re != nil {
		t.Fatal("an invalid regex must not compile")
	}
	if !bad.match("web-(1)") {
		t.Error("an invalid regex should fall back to a substring match")
	}
	// Case insensitivity is what the substring filter did, so the regex keeps it.
	if !newRowOpts("WEB", false, false).match("web-1") {
		t.Error("filter should be case insensitive")
	}
}

// `-l app=web` is kubectl syntax and must reach the API server, not be matched
// as text against the rows.
func TestLabelSelectorIsSplitOutOfTheFilter(t *testing.T) {
	m := New(browserTargets())
	m.filter = "-l app=web"
	selector, filter := m.selectorAndFilter()
	if selector != "app=web" || filter != "" {
		t.Errorf("selector=%q filter=%q; want app=web and an empty filter", selector, filter)
	}
	m.filter = "web"
	if selector, filter := m.selectorAndFilter(); selector != "" || filter != "web" {
		t.Errorf("plain filter split wrongly: %q / %q", selector, filter)
	}
}

// Ages sort by duration: as text, "3d" sorts before "12m".
func TestSortByAgeUsesDurationNotText(t *testing.T) {
	rows := podRows(samplePods(), rowOpts{})
	sortRows(rows, viewPods, sortAge, false)
	if rows[0][6] != "1m" || rows[len(rows)-1][6] != "2d" {
		t.Errorf("age sort = %v", []string{rows[0][6], rows[1][6], rows[2][6]})
	}
	sortRows(rows, viewPods, sortAge, true)
	if rows[0][6] != "2d" {
		t.Errorf("reversed age sort put %q first", rows[0][6])
	}
}

func TestParseShortAge(t *testing.T) {
	cases := map[string]time.Duration{
		"30s": 30 * time.Second, "12m": 12 * time.Minute,
		"4h": 4 * time.Hour, "3d": 72 * time.Hour, "": 0, "junk": 0,
	}
	for in, want := range cases {
		if got := parseShortAge(in); got != want {
			t.Errorf("parseShortAge(%q) = %v; want %v", in, got, want)
		}
	}
}

// Pressing the same sort key twice reverses; a different key starts ascending.
func TestSortKeyTogglesDirection(t *testing.T) {
	m := New(browserTargets())
	m.view = viewPods
	m.pods = samplePods()
	m.rebuildTable()
	m = send(m, "N")
	if m.sortBy != sortName || m.sortRev {
		t.Fatalf("first N: sortBy=%v rev=%v", m.sortBy, m.sortRev)
	}
	m = send(m, "N")
	if !m.sortRev {
		t.Error("second N should reverse")
	}
	m = send(m, "A")
	if m.sortBy != sortAge || m.sortRev {
		t.Errorf("A should switch to age, ascending: %v/%v", m.sortBy, m.sortRev)
	}
}

func TestWideAndFaultsTogglesAreScopedToTables(t *testing.T) {
	m := New(browserTargets())
	m.view = viewPods
	m.pods = samplePods()
	m.rebuildTable()
	m = send(m, "ctrl+w")
	if !m.wide {
		t.Error("ctrl-w should turn wide mode on")
	}
	m = send(m, "ctrl+z")
	if !m.faultsOnly {
		t.Error("ctrl-z should turn the faults filter on")
	}
	// The dashboard has no table, so the toggles must not silently flip there.
	d := New(browserTargets())
	d = send(d, "ctrl+w")
	if d.wide {
		t.Error("ctrl-w on the dashboard should do nothing")
	}
}

func TestDoctorRowsAndDetail(t *testing.T) {
	ds := []troubleshoot.Diagnosis{
		{SignatureID: "pod.imagepull", Title: "Image cannot be pulled", Confidence: 90,
			Remediation: "check the registry credentials"},
		{SignatureID: "node.diskpressure", Title: "Disk is nearly full", Confidence: 60},
	}
	rows := doctorRows(ds, rowOpts{})
	if len(rows) != 2 || rows[0][0] != "90%" || rows[0][1] != "pod.imagepull" {
		t.Fatalf("doctor rows = %v", rows)
	}
	detail := doctorDetail(ds[0])
	for _, want := range []string{"pod.imagepull", "90%", "check the registry credentials"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail missing %q:\n%s", want, detail)
		}
	}
	if !strings.Contains(doctorSummary(nil), "healthy") {
		t.Error("no findings should read as healthy")
	}
	if !strings.Contains(doctorSummary(ds), "2 finding") {
		t.Errorf("summary = %q", doctorSummary(ds))
	}
}

// A doctor pass that gathered nothing must not be shown as a clean bill of
// health: the probe errors have to reach the screen.
func TestDoctorViewShowsProbeErrors(t *testing.T) {
	m := New(browserTargets())
	m.view = viewDoctor
	m.loading = false
	m.probeErrors = map[string]string{"pods": "connection refused"}
	out := m.View()
	if !strings.Contains(out, "could not gather pods") {
		t.Errorf("probe errors not surfaced:\n%s", out)
	}
}

func TestSelectedDiagnosisFindsTheRow(t *testing.T) {
	m := New(browserTargets())
	m.view = viewDoctor
	m.diagnoses = []troubleshoot.Diagnosis{
		{SignatureID: "a.one", Title: "One", Confidence: 80},
		{SignatureID: "b.two", Title: "Two", Confidence: 40},
	}
	m.rebuildTable()
	m = send(m, "down")
	d, ok := m.selectedDiagnosis()
	if !ok || d.SignatureID != "b.two" {
		t.Fatalf("selected %+v (ok=%v); want b.two", d, ok)
	}
	m = send(m, "enter")
	if m.view != viewDescribe || !strings.Contains(m.viewport.View(), "Two") {
		t.Errorf("enter should open the finding's detail pane; view=%q", m.view)
	}
}

// Drilling from a workload into its pods uses the workload's own selector, so
// the pod list is exact rather than a name-prefix guess.
func TestWorkloadDrillDownUsesSelector(t *testing.T) {
	m := New(browserTargets())
	m.view = viewDeployments
	m.workloads = sampleWorkloads()
	m.rebuildTable()
	m = send(m, "enter") // first row is "api" (sorted by name)
	if m.view != viewPods {
		t.Fatalf("view = %q, want pods", m.view)
	}
	if m.filter != "-l app=api" {
		t.Errorf("filter = %q; want the workload's label selector", m.filter)
	}
	if m.namespace != "prod" {
		t.Errorf("namespace = %q; want the workload's namespace", m.namespace)
	}
}

func TestGenCommandOpensStudio(t *testing.T) {
	m := New(browserTargets())
	m = send(m, ":")
	m = typeString(m, "gen deployment web nginx:1.27")
	next, cmd := m.runCommand("gen deployment web nginx:1.27")
	m = next.(Model)
	if cmd == nil {
		t.Fatal("gen should return a command")
	}
	msg := cmd()
	gen, ok := msg.(genMsg)
	if !ok {
		t.Fatalf("got %T, want genMsg", msg)
	}
	if gen.err != nil {
		t.Fatalf("gen: %v", gen.err)
	}
	if !strings.Contains(gen.yaml, "kind: Deployment") || !strings.Contains(gen.yaml, "nginx:1.27") {
		t.Errorf("generated manifest:\n%s", gen.yaml)
	}
	// The studio verifies what it generated, and says so.
	if !strings.Contains(gen.body, "verify") {
		t.Errorf("studio pane does not report the verify result:\n%s", gen.body)
	}
}

func TestParseGenArgs(t *testing.T) {
	p, err := parseGenArgs("deployment web nginx:1.27 3 8080")
	if err != nil {
		t.Fatalf("parseGenArgs: %v", err)
	}
	if p.Kind != "deployment" || p.Name != "web" || p.Image != "nginx:1.27" || p.Replicas != 3 || p.Port != 8080 {
		t.Errorf("parsed %+v", p)
	}
	if _, err := parseGenArgs("deployment"); err == nil {
		t.Error("a missing name should be an error with usage")
	}
	if _, err := parseGenArgs("deployment web nginx three"); err == nil {
		t.Error("a non-numeric replica count should be rejected")
	}
}

// The studio is a scratchpad; it must never overwrite a manifest someone else
// wrote.
func TestSaveGenRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	p, err := parseGenArgs("deployment web")
	if err != nil {
		t.Fatal(err)
	}
	msg := saveGen("kind: Deployment\n", p)().(savedMsg)
	if msg.err != nil {
		t.Fatalf("first save: %v", msg.err)
	}
	if filepath.Base(msg.path) != "web-deployment.yaml" {
		t.Errorf("saved to %q", msg.path)
	}
	again := saveGen("kind: Deployment\n", p)().(savedMsg)
	if again.err == nil {
		t.Fatal("a second save must refuse rather than overwrite")
	}
}

func TestRolloutFraction(t *testing.T) {
	if got := rolloutFraction(nil); got != 1 {
		t.Errorf("nothing applied = %v; want a full bar", got)
	}
}

func TestFirstPortReadsServicePortColumn(t *testing.T) {
	cases := map[string]int{
		"80/TCP":            80,
		"443:30443/TCP":     443,
		"8080/TCP,8443/TCP": 8080,
		"":                  80,
		"nonsense":          80,
	}
	for in, want := range cases {
		if got := firstPort(in); got != want {
			t.Errorf("firstPort(%q) = %d; want %d", in, got, want)
		}
	}
}

func TestVMRowsReportUnreachableNodes(t *testing.T) {
	nodes := []vmNode{
		{Name: "n1", Role: "server", Address: "10.0.0.1", SSH: "ok", Version: "v1.31.2+k3s1", Ready: true},
		{Name: "n2", Role: "agent", Address: "10.0.0.2", SSH: "unreachable", Version: "dial timeout"},
	}
	rows := vmRows(nodes, rowOpts{})
	if len(rows) != 2 {
		t.Fatalf("got %d rows", len(rows))
	}
	// ctrl-z on the targets view keeps the nodes that need attention.
	faulty := vmRows(nodes, newRowOpts("", false, true))
	if len(faulty) != 1 || faulty[0][0] != "n2" {
		t.Errorf("faults-only vm rows = %v", faulty)
	}
}

func TestFirstVersion(t *testing.T) {
	if got := firstVersion("k3s version v1.31.2+k3s1 (a1b2c3)"); got != "v1.31.2+k3s1" {
		t.Errorf("firstVersion = %q", got)
	}
	if got := firstVersion("Kubernetes v1.31.2"); got != "v1.31.2" {
		t.Errorf("firstVersion = %q", got)
	}
	if firstVersion("command not found") != "" {
		t.Error("no version token should render empty")
	}
}

func TestBootstrapFraction(t *testing.T) {
	lines := []string{"[10.0.0.1] installing k3s server — cluster-init...", "[10.0.0.2] joining as server (etcd member)..."}
	if got := bootstrapFraction(lines, 3); got < 0.66 || got > 0.67 {
		t.Errorf("fraction = %v; want 2/3", got)
	}
	if got := bootstrapFraction(nil, 0); got != 1 {
		t.Errorf("no nodes = %v; want a full bar", got)
	}
	// Never past full, however many lines arrive.
	if got := bootstrapFraction([]string{"installing", "installing", "joining"}, 1); got != 1 {
		t.Errorf("fraction = %v; want it capped at 1", got)
	}
}

func TestCtxRowsMarkTheCurrentCluster(t *testing.T) {
	f := &config.File{Clusters: []config.Targets{
		{Cluster: "prod", Nodes: []config.Node{{Name: "p1", Role: "server", Host: "10.0.0.1", User: "u"}}},
		{Cluster: "staging", Nodes: []config.Node{{Name: "s1", Role: "server", Host: "10.0.1.1", User: "u"}}},
	}}
	rows := ctxRows(f, "staging", rowOpts{})
	if len(rows) != 2 {
		t.Fatalf("got %d rows", len(rows))
	}
	for _, r := range rows {
		marked := strings.Contains(r[0], "●")
		if (r[1] == "staging") != marked {
			t.Errorf("row %v: marker does not follow the current cluster", r)
		}
	}
}

// :ctx without a loaded file is an error rather than an empty table, so the
// operator is told why there is nothing to switch to.
func TestCtxWithoutFileReportsWhy(t *testing.T) {
	m := New(browserTargets())
	next, _ := m.runCommand("ctx")
	m = next.(Model)
	if m.err == nil || !strings.Contains(m.err.Error(), "multi-cluster") {
		t.Errorf("err = %v", m.err)
	}
	if m.view == viewCtx {
		t.Error("should not switch to a view it cannot populate")
	}
}

func TestSwitchContextClearsTheOldClusterState(t *testing.T) {
	f := &config.File{Clusters: []config.Targets{
		{Cluster: "prod", Nodes: []config.Node{{Name: "p1", Role: "server", Host: "10.0.0.1", User: "u"}}},
		{Cluster: "staging", Nodes: []config.Node{{Name: "s1", Role: "server", Host: "10.0.1.1", User: "u"}}},
	}}
	m := New(&config.Targets{Cluster: "prod", Nodes: f.Clusters[0].Nodes}).WithFile(f, "targets.yaml")
	m.view = viewCtx
	m.pods = samplePods()
	m.diagnoses = []troubleshoot.Diagnosis{{SignatureID: "x", Confidence: 50}}
	m.rebuildTable()
	m = send(m, "down") // staging
	m = send(m, "enter")
	if m.targets.Cluster != "staging" {
		t.Fatalf("cluster = %q, want staging", m.targets.Cluster)
	}
	// Anything left over would be shown under the new cluster's name.
	if m.pods != nil || m.diagnoses != nil || m.server != nil {
		t.Error("switching context must drop the previous cluster's state")
	}
	if m.view != viewDashboard {
		t.Errorf("view = %q; want the dashboard for the new cluster", m.view)
	}
}

func TestRenderTree(t *testing.T) {
	roots := []*kube.TreeNode{{
		Kind: "Deployment", Name: "web", Namespace: "prod", Status: "1/1 ready", Healthy: true,
		Children: []*kube.TreeNode{{
			Kind: "ReplicaSet", Name: "web-abc", Namespace: "prod", Status: "1/1 ready", Healthy: true,
			Children: []*kube.TreeNode{
				{Kind: "Pod", Name: "web-abc-1", Namespace: "prod", Status: "Running", Healthy: true},
			},
		}},
	}}
	out := renderTree(roots)
	for _, want := range []string{"prod", "deployment", "web-abc", "web-abc-1", "└─"} {
		if !strings.Contains(out, want) {
			t.Errorf("tree missing %q:\n%s", want, out)
		}
	}
	if renderTree(nil) != "(no workloads)" {
		t.Error("an empty graph should say so")
	}
}

func TestThemes(t *testing.T) {
	t.Cleanup(func() { applyTheme(builtinThemes["dark"]) })
	for _, name := range ThemeNames() {
		if err := SetTheme(name); err != nil {
			t.Fatalf("SetTheme(%q): %v", name, err)
		}
		if activeTheme.Name != name {
			t.Errorf("active theme = %q, want %q", activeTheme.Name, name)
		}
	}
	// A typo must be reported, not silently ignored.
	if err := SetTheme("drak"); err == nil {
		t.Error("an unknown theme should be an error")
	}
	// A skin file with only some fields set keeps the rest of the defaults,
	// so a partial skin cannot erase the ok/warn/fail distinction.
	path := filepath.Join(t.TempDir(), "mine.yaml")
	if err := os.WriteFile(path, []byte("accent: \"33\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetTheme(path); err != nil {
		t.Fatalf("SetTheme(file): %v", err)
	}
	if activeTheme.Name != "mine" {
		t.Errorf("theme name = %q; want the file's stem", activeTheme.Name)
	}
	// Compared on the style's colour rather than its rendered output: a test
	// binary has no TTY, so lipgloss strips the escape codes and every style
	// would render identically.
	if statusOKStyle.GetForeground() == statusFailStyle.GetForeground() {
		t.Error("a partial skin must not collapse ok and fail to the same colour")
	}
	if accentColor != lipgloss.Color("33") {
		t.Errorf("the skin file's accent was not applied: %v", accentColor)
	}
}

func TestThemeCommandSwitchesLive(t *testing.T) {
	t.Cleanup(func() { applyTheme(builtinThemes["dark"]) })
	m := New(browserTargets())
	next, _ := m.runCommand("theme k3s-orange")
	m = next.(Model)
	if m.err != nil {
		t.Fatalf("theme command: %v", m.err)
	}
	if activeTheme.Name != "k3s-orange" {
		t.Errorf("active theme = %q", activeTheme.Name)
	}
	next, _ = m.runCommand("theme nope")
	if next.(Model).err == nil {
		t.Error("an unknown theme should surface an error in the TUI too")
	}
}

// The sparkline is scaled 0-100, not to the sample range: a flat 3% and a flat
// 98% must not draw the same graph.
func TestSparklineFixedScale(t *testing.T) {
	low := sparkline([]float64{3, 3, 3}, 3)
	high := sparkline([]float64{98, 98, 98}, 3)
	if low == high {
		t.Error("a quiet node and a saturated node drew the same sparkline")
	}
	if got := sparkline(nil, 10); got != "" {
		t.Errorf("no samples should draw nothing, got %q", got)
	}
	// Only the most recent `width` samples are drawn.
	if len([]rune(stripANSI(sparkline([]float64{1, 2, 3, 4, 5}, 3)))) != 3 {
		t.Error("sparkline should be clipped to its width")
	}
}

func TestParseHostSample(t *testing.T) {
	out := "1.00 0.80 0.75 2/500 9999\n4\nMemTotal:       8000000 kB\nMemFree:         100000 kB\nMemAvailable:   2000000 kB\n"
	s := parseHostSample(out)
	if !s.OK {
		t.Fatal("a complete probe should parse")
	}
	if s.CPU < 24 || s.CPU > 26 {
		t.Errorf("cpu = %v; want load 1.00 over 4 cores = 25%%", s.CPU)
	}
	// MemAvailable, not MemFree: page cache is not "used".
	if s.Mem < 74 || s.Mem > 76 {
		t.Errorf("mem = %v; want 75%%", s.Mem)
	}
}

// An unreadable probe must not enter the history as 0%: a flat healthy line
// for a node under pressure is the failure mode this guards.
func TestFailedSampleIsNotRecorded(t *testing.T) {
	if s := parseHostSample("garbage"); s.OK {
		t.Fatal("garbage should not parse as a sample")
	}
	h := newHistory()
	h.push("n1", hostSample{CPU: 0, Mem: 0, OK: false})
	if len(h.cpuFor("n1")) != 0 {
		t.Error("a failed probe must not be stored")
	}
	h.push("n1", hostSample{CPU: 50, Mem: 60, OK: true})
	if v, ok := latest(h.cpuFor("n1")); !ok || v != 50 {
		t.Errorf("latest = %v,%v", v, ok)
	}
}

func TestHistoryIsCapped(t *testing.T) {
	h := newHistory()
	for i := 0; i < historyLen*2; i++ {
		h.push("n1", hostSample{CPU: float64(i), Mem: 1, OK: true})
	}
	if got := len(h.cpuFor("n1")); got != historyLen {
		t.Errorf("history holds %d samples, want %d", got, historyLen)
	}
	if v, _ := latest(h.cpuFor("n1")); v != float64(historyLen*2-1) {
		t.Errorf("newest sample = %v", v)
	}
}

// The dashboard's score and sparklines are what the status bar reports, so
// they have to survive a render.
func TestDashboardShowsScoreAndGraphs(t *testing.T) {
	m := New(browserTargets())
	m.loading = false
	m.results = map[string][]check.Result{"node1": {
		{ID: "host.disk", Name: "Disk", Status: check.OK, Summary: "40% used"},
		{ID: "host.mem", Name: "Memory", Status: check.Fail, Summary: "95% used"},
	}}
	m.hist.push("node1", hostSample{CPU: 12, Mem: 40, OK: true})
	score, have := m.healthScore()
	if !have || score != 50 {
		t.Fatalf("score = %d,%v; want 50%%", score, have)
	}
	out := m.View()
	if !strings.Contains(out, "50%") {
		t.Errorf("score missing from the dashboard:\n%s", out)
	}
	if !strings.Contains(out, "cpu") || !strings.Contains(out, "mem") {
		t.Errorf("node card is missing its load graphs:\n%s", out)
	}
	// The status bar carries the score on every view, not just the dashboard.
	m.view = viewPods
	m.pods = samplePods()
	m.rebuildTable()
	if !strings.Contains(m.View(), "score 50%") {
		t.Error("status bar should always carry the health score")
	}
}

func TestHighlightersPreserveContent(t *testing.T) {
	yamlDoc := "# comment\napiVersion: apps/v1\nkind: Deployment\n"
	if got := stripANSI(highlightYAML(yamlDoc)); got != strings.TrimRight(yamlDoc, "\n") {
		t.Errorf("highlighting changed the text:\n%q\n%q", got, yamlDoc)
	}
	describe := "Name:  web\nEvents:\n  Warning  Failed  pull image\n"
	if got := stripANSI(highlightDescribe(describe)); got != strings.TrimRight(describe, "\n") {
		t.Errorf("describe highlighting changed the text:\n%q", got)
	}
}

func TestColorDiffKeepsEveryLine(t *testing.T) {
	diff := "--- live\n+++ new\n@@ -1 +1 @@\n-  replicas: 1\n+  replicas: 3\n context"
	if got := stripANSI(colorDiff(diff)); got != diff {
		t.Errorf("diff colouring changed the text:\n%q", got)
	}
}

// stripANSI removes styling so tests can compare rendered text to its source.
func stripANSI(s string) string {
	var b strings.Builder
	inEscape := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			inEscape = true
		case inEscape && (r == 'm' || r == 'K'):
			inEscape = false
		case !inEscape:
			b.WriteRune(r)
		}
	}
	return b.String()
}
