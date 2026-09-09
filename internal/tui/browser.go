package tui

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/solutionforest/k3helper/internal/kube"
	"github.com/solutionforest/k3helper/internal/ssh"
)

// view identifies what the browser is currently showing.
type view string

const (
	viewDashboard    view = "dashboard"
	viewPods         view = "pods"
	viewNodes        view = "nodes"
	viewEvents       view = "events"
	viewLogs         view = "logs"
	viewDescribe     view = "describe"
	viewPorts        view = "ports"
	viewMultiLogs    view = "multi-logs"
	viewDeployments  view = "deployments"
	viewStatefulSets view = "statefulsets"
	viewDaemonSets   view = "daemonsets"
	viewServices     view = "services"
	viewIngresses    view = "ingresses"
	viewDoctor       view = "doctor"
	viewXray         view = "xray"
	viewVM           view = "vm"
	viewCtx          view = "contexts"
	viewGen          view = "gen"
	viewDeploy       view = "deploy"
)

// tableViews are the views backed by a table rather than a text pane.
func (v view) isTable() bool {
	switch v {
	case viewPods, viewNodes, viewEvents, viewPorts, viewDeployments, viewStatefulSets,
		viewDaemonSets, viewServices, viewIngresses, viewDoctor, viewVM, viewCtx:
		return true
	}
	return false
}

// isText reports whether the view renders into the scrollable pane.
func (v view) isText() bool {
	switch v {
	case viewLogs, viewDescribe, viewMultiLogs, viewXray, viewGen, viewDeploy:
		return true
	}
	return false
}

// isWorkload reports whether the view lists deployments/statefulsets/daemonsets,
// which share a row shape and a drill-down.
func (v view) isWorkload() bool {
	return v == viewDeployments || v == viewStatefulSets || v == viewDaemonSets
}

// resolveView maps a command-bar word to a view, accepting the k9s-style
// short aliases operators already have in their fingers.
func resolveView(s string) (view, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "po", "pod", "pods":
		return viewPods, true
	case "no", "node", "nodes":
		return viewNodes, true
	case "ev", "event", "events":
		return viewEvents, true
	case "dash", "dashboard", "home":
		return viewDashboard, true
	case "pf", "port", "ports", "port-forward":
		return viewPorts, true
	case "logs", "log", "tail":
		return viewMultiLogs, true
	case "dp", "deploy", "deploys", "deployment", "deployments":
		// `:deploy` with no argument is k9s's deployments list. `:deploy <file>`
		// is our apply flow, and is routed before this by runCommand.
		return viewDeployments, true
	case "sts", "statefulset", "statefulsets":
		return viewStatefulSets, true
	case "ds", "daemonset", "daemonsets":
		return viewDaemonSets, true
	case "svc", "service", "services":
		return viewServices, true
	case "ing", "ingress", "ingresses":
		return viewIngresses, true
	case "doctor", "dr", "diagnose":
		return viewDoctor, true
	case "xray", "x", "tree":
		return viewXray, true
	case "vm", "vms", "targets":
		return viewVM, true
	case "ctx", "context", "contexts", "cluster", "clusters":
		return viewCtx, true
	}
	return "", false
}

// resourcesMsg carries a completed resource fetch.
type resourcesMsg struct {
	view      view
	pods      []kube.Pod
	nodes     []kube.Node
	events    []kube.Event
	workloads []kube.Workload
	services  []kube.Service
	ingresses []kube.Ingress
	err       error
}

// textMsg carries logs or describe output.
type textMsg struct {
	view  view
	title string
	body  string
	err   error
}

// fetchResources loads the data backing a table view.
//
// selector is a label selector when the operator typed `-l ...` into the
// filter bar, and is applied by the API server; the plain filter is applied to
// the rows afterwards.
func fetchResources(exec kube.Executor, v view, namespace, selector string) tea.Cmd {
	return func() tea.Msg {
		if exec == nil {
			return resourcesMsg{view: v, err: fmt.Errorf("no server connection")}
		}
		switch v {
		case viewPods:
			pods, err := kube.ListPodsSelector(exec, namespace, selector)
			return resourcesMsg{view: v, pods: pods, err: err}
		case viewNodes:
			nodes, err := kube.ListNodes(exec)
			return resourcesMsg{view: v, nodes: nodes, err: err}
		case viewEvents:
			events, err := kube.ListEvents(exec, namespace)
			return resourcesMsg{view: v, events: events, err: err}
		case viewDeployments, viewStatefulSets, viewDaemonSets:
			w, err := kube.ListWorkloads(exec, string(v), namespace)
			return resourcesMsg{view: v, workloads: w, err: err}
		case viewServices:
			s, err := kube.ListServices(exec, namespace)
			return resourcesMsg{view: v, services: s, err: err}
		case viewIngresses:
			i, err := kube.ListIngresses(exec, namespace)
			return resourcesMsg{view: v, ingresses: i, err: err}
		}
		return resourcesMsg{view: v}
	}
}

// fetchLogs loads a pod's logs, falling back to the previous container
// instance when the current one has produced nothing yet.
func fetchLogs(exec kube.Executor, ns, pod string) tea.Cmd {
	return func() tea.Msg {
		title := fmt.Sprintf("logs %s/%s", ns, pod)
		if exec == nil {
			return textMsg{view: viewLogs, title: title, err: fmt.Errorf("no server connection")}
		}
		body, err := kube.Logs(exec, ns, pod, 300, false)
		if err == nil && strings.TrimSpace(body) == "" {
			// A crashlooping pod's current instance is often empty; the
			// interesting output is in the instance that died.
			if prev, perr := kube.Logs(exec, ns, pod, 300, true); perr == nil && strings.TrimSpace(prev) != "" {
				return textMsg{view: viewLogs, title: title + " (previous instance)", body: prev}
			}
			body = "(no log output)"
		}
		return textMsg{view: viewLogs, title: title, body: body, err: err}
	}
}

// fetchDescribe loads `kubectl describe` for the selected object.
func fetchDescribe(exec kube.Executor, kind, ns, name string) tea.Cmd {
	return func() tea.Msg {
		title := fmt.Sprintf("describe %s %s", kind, name)
		if exec == nil {
			return textMsg{view: viewDescribe, title: title, err: fmt.Errorf("no server connection")}
		}
		body, err := kube.Describe(exec, kind, ns, name)
		if err == nil {
			body = highlightDescribe(body)
		}
		return textMsg{view: viewDescribe, title: title, body: body, err: err}
	}
}

// --- table construction -----------------------------------------------------

func newTable(cols []table.Column, rows []table.Row, height int) table.Model {
	if height < 3 {
		height = 3
	}
	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithFocused(true),
		table.WithHeight(height),
	)
	s := table.DefaultStyles()
	s.Header = s.Header.Bold(true).Foreground(accentColor).BorderBottom(true)
	s.Selected = s.Selected.Bold(true).Foreground(selectedFg).Background(selectedBg)
	t.SetStyles(s)
	return t
}

// rowOpts is everything that decides which rows are built and how wide they
// are: the filter, wide mode (ctrl-w) and the faults-only toggle (ctrl-z).
type rowOpts struct {
	filter     string
	re         *regexp.Regexp
	wide       bool
	faultsOnly bool
}

// newRowOpts compiles the filter as a regular expression, falling back to a
// substring match when it does not compile.
//
// Falling back rather than erroring keeps a half-typed regex usable: live
// filtering sees "web-(" on the way to "web-(a|b)", and refusing to filter at
// all in between would blank the table on every keystroke.
func newRowOpts(filter string, wide, faultsOnly bool) rowOpts {
	o := rowOpts{filter: filter, wide: wide, faultsOnly: faultsOnly}
	if filter == "" {
		return o
	}
	// (?i) so a filter behaves like the substring match it replaced.
	if re, err := regexp.Compile("(?i)" + filter); err == nil {
		o.re = re
	}
	return o
}

// match reports whether any field matches the active filter.
func (o rowOpts) match(fields ...string) bool {
	if o.filter == "" {
		return true
	}
	if o.re != nil {
		for _, v := range fields {
			if o.re.MatchString(v) {
				return true
			}
		}
		return false
	}
	return matchesFilter(o.filter, fields...)
}

func podRows(pods []kube.Pod, o rowOpts) []table.Row {
	rows := make([]table.Row, 0, len(pods))
	for _, p := range pods {
		if !o.match(p.Namespace, p.Name, p.Status, p.Node) {
			continue
		}
		if o.faultsOnly && p.Healthy() {
			continue
		}
		row := table.Row{
			p.Namespace, p.Name, p.Ready, p.Status,
			fmt.Sprint(p.Restarts), p.Node, kube.ShortAge(p.Age),
		}
		if o.wide {
			ip := p.IP
			if ip == "" {
				ip = "<none>"
			}
			row = append(row, ip, renderLabels(p.Labels))
		}
		rows = append(rows, row)
	}
	return rows
}

func nodeRows(nodes []kube.Node, o rowOpts) []table.Row {
	rows := make([]table.Row, 0, len(nodes))
	for _, n := range nodes {
		if !o.match(n.Name, n.Status, n.Roles) {
			continue
		}
		if o.faultsOnly && n.Status == "Ready" {
			continue
		}
		row := table.Row{n.Name, n.Status, n.Roles, n.Version, kube.ShortAge(n.Age)}
		if o.wide {
			row = append(row, n.InternalIP, n.OSImage, n.Kernel)
		}
		rows = append(rows, row)
	}
	return rows
}

func eventRows(events []kube.Event, o rowOpts) []table.Row {
	rows := make([]table.Row, 0, len(events))
	for _, e := range events {
		if !o.match(e.Namespace, e.Reason, e.Object, e.Message, e.Type) {
			continue
		}
		if o.faultsOnly && e.Type != "Warning" {
			continue
		}
		msgWidth := 60
		if o.wide {
			msgWidth = 160
		}
		rows = append(rows, table.Row{
			kube.ShortAge(e.Age), e.Type, e.Reason, e.Namespace, e.Object, truncate(e.Message, msgWidth),
		})
	}
	return rows
}

func workloadRows(ws []kube.Workload, o rowOpts) []table.Row {
	rows := make([]table.Row, 0, len(ws))
	for _, w := range ws {
		if !o.match(w.Namespace, w.Name, w.Images) {
			continue
		}
		if o.faultsOnly && w.Healthy() {
			continue
		}
		row := table.Row{
			w.Namespace, w.Name, w.Ready,
			fmt.Sprint(w.UpToDate), fmt.Sprint(w.Available), kube.ShortAge(w.Age),
		}
		if o.wide {
			row = append(row, truncate(w.Images, 40), truncate(w.Selector, 30))
		}
		rows = append(rows, row)
	}
	return rows
}

func serviceRows(svcs []kube.Service, o rowOpts) []table.Row {
	rows := make([]table.Row, 0, len(svcs))
	for _, s := range svcs {
		if !o.match(s.Namespace, s.Name, s.Type, s.ClusterIP, s.Ports) {
			continue
		}
		// A LoadBalancer with no address is the one service state that is
		// waiting on something; nothing else here has a health notion.
		if o.faultsOnly && s.ExternalIP != "<pending>" {
			continue
		}
		row := table.Row{s.Namespace, s.Name, s.Type, s.ClusterIP, s.ExternalIP, s.Ports, kube.ShortAge(s.Age)}
		if o.wide {
			row = append(row, truncate(s.Selector, 34))
		}
		rows = append(rows, row)
	}
	return rows
}

func ingressRows(ings []kube.Ingress, o rowOpts) []table.Row {
	rows := make([]table.Row, 0, len(ings))
	for _, i := range ings {
		if !o.match(i.Namespace, i.Name, i.Hosts, i.Class, i.Address) {
			continue
		}
		// An ingress with no address has no controller behind it yet.
		if o.faultsOnly && i.Address != "" {
			continue
		}
		addr := i.Address
		if addr == "" {
			addr = "<pending>"
		}
		rows = append(rows, table.Row{
			i.Namespace, i.Name, i.Class, truncate(i.Hosts, 30), addr, i.Ports, kube.ShortAge(i.Age),
		})
	}
	return rows
}

// renderLabels flattens labels for the wide pod column, keys sorted.
func renderLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "<none>"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return truncate(strings.Join(parts, ","), 40)
}

// matchesFilter reports whether any field contains the filter, case
// insensitively. An empty filter matches everything.
func matchesFilter(filter string, fields ...string) bool {
	if filter == "" {
		return true
	}
	f := strings.ToLower(filter)
	for _, v := range fields {
		if strings.Contains(strings.ToLower(v), f) {
			return true
		}
	}
	return false
}

// --- sorting -----------------------------------------------------------------

// sortKey is the column a table is ordered by (k9s: shift-n/a/s).
type sortKey int

const (
	sortNone sortKey = iota
	sortName
	sortAge
	sortStatus
)

func (s sortKey) String() string {
	switch s {
	case sortName:
		return "name"
	case sortAge:
		return "age"
	case sortStatus:
		return "status"
	}
	return ""
}

// sortColumn is the row index the key sorts on for a given view, and whether
// that column holds a duration rendered as "3d"/"12m".
func sortColumn(v view, k sortKey) (idx int, isAge bool, ok bool) {
	switch v {
	case viewPods:
		switch k {
		case sortName:
			return 1, false, true
		case sortStatus:
			return 3, false, true
		case sortAge:
			return 6, true, true
		}
	case viewNodes:
		switch k {
		case sortName:
			return 0, false, true
		case sortStatus:
			return 1, false, true
		case sortAge:
			return 4, true, true
		}
	case viewEvents:
		switch k {
		case sortName:
			return 4, false, true
		case sortStatus:
			return 1, false, true
		case sortAge:
			return 0, true, true
		}
	case viewDeployments, viewStatefulSets, viewDaemonSets:
		switch k {
		case sortName:
			return 1, false, true
		case sortStatus:
			return 2, false, true
		case sortAge:
			return 5, true, true
		}
	case viewServices:
		switch k {
		case sortName:
			return 1, false, true
		case sortStatus:
			return 2, false, true
		case sortAge:
			return 6, true, true
		}
	case viewIngresses:
		switch k {
		case sortName:
			return 1, false, true
		case sortStatus:
			return 4, false, true
		case sortAge:
			return 6, true, true
		}
	case viewDoctor:
		switch k {
		case sortName:
			return 2, false, true
		case sortStatus:
			// The doctor's "status" is its confidence, which is what makes a
			// finding worth reading first.
			return 0, false, true
		}
	}
	return 0, false, false
}

// sortRows orders rows in place by the active key.
//
// Ages sort by the duration they represent, not the string: "3d" and "12m"
// compare as text in the wrong order, which would put the newest pod between
// two of the oldest.
func sortRows(rows []table.Row, v view, k sortKey, reverse bool) {
	idx, isAge, ok := sortColumn(v, k)
	if !ok || k == sortNone {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if idx >= len(rows[i]) || idx >= len(rows[j]) {
			return false
		}
		a, b := rows[i][idx], rows[j][idx]
		var less bool
		if isAge {
			less = parseShortAge(a) < parseShortAge(b)
		} else {
			less = strings.ToLower(a) < strings.ToLower(b)
		}
		if reverse {
			return !less
		}
		return less
	})
}

// parseShortAge inverts kube.ShortAge for sorting.
func parseShortAge(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	unit := s[len(s)-1]
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil {
		return 0
	}
	switch unit {
	case 's':
		return time.Duration(n) * time.Second
	case 'm':
		return time.Duration(n) * time.Minute
	case 'h':
		return time.Duration(n) * time.Hour
	case 'd':
		return time.Duration(n) * 24 * time.Hour
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// columnsFor returns the table columns for a view, scaled to the terminal
// width so long names are not clipped on a wide screen. wide adds the extra
// columns ctrl-w reveals, and must agree with the row builders.
func columnsFor(v view, width int, wide bool) []table.Column {
	if width < 80 {
		width = 80
	}
	switch v {
	case viewPods:
		name := width - 16 - 6 - 18 - 5 - 14 - 6 - 8
		if wide {
			name -= 50
		}
		cols := []table.Column{
			{Title: "NAMESPACE", Width: 16},
			{Title: "NAME", Width: clamp(name, 20, 60)},
			{Title: "READY", Width: 6},
			{Title: "STATUS", Width: 18},
			{Title: "RST", Width: 5},
			{Title: "NODE", Width: 14},
			{Title: "AGE", Width: 6},
		}
		if wide {
			cols = append(cols,
				table.Column{Title: "IP", Width: 15},
				table.Column{Title: "LABELS", Width: clamp(width-100, 12, 40)})
		}
		return cols
	case viewNodes:
		cols := []table.Column{
			{Title: "NAME", Width: clamp(width-46, 16, 40)},
			{Title: "STATUS", Width: 10},
			{Title: "ROLES", Width: 16},
			{Title: "VERSION", Width: 16},
			{Title: "AGE", Width: 6},
		}
		if wide {
			cols = append(cols,
				table.Column{Title: "INTERNAL-IP", Width: 15},
				table.Column{Title: "OS-IMAGE", Width: clamp(width-110, 12, 30)},
				table.Column{Title: "KERNEL", Width: clamp(width-110, 12, 24)})
		}
		return cols
	case viewPorts:
		return []table.Column{
			{Title: "LOCAL", Width: 8},
			{Title: "NAMESPACE", Width: 16},
			{Title: "TARGET", Width: clamp(width-50, 16, 40)},
			{Title: "PORT", Width: 8},
		}
	case viewEvents:
		msg := width - 6 - 9 - 20 - 14 - 26 - 8
		return []table.Column{
			{Title: "AGE", Width: 6},
			{Title: "TYPE", Width: 9},
			{Title: "REASON", Width: 20},
			{Title: "NAMESPACE", Width: 14},
			{Title: "OBJECT", Width: 26},
			{Title: "MESSAGE", Width: clamp(msg, 20, 160)},
		}
	case viewDeployments, viewStatefulSets, viewDaemonSets:
		name := width - 16 - 8 - 10 - 11 - 6 - 8
		if wide {
			name -= 74
		}
		cols := []table.Column{
			{Title: "NAMESPACE", Width: 16},
			{Title: "NAME", Width: clamp(name, 20, 60)},
			{Title: "READY", Width: 8},
			{Title: "UP-TO-DATE", Width: 10},
			{Title: "AVAILABLE", Width: 11},
			{Title: "AGE", Width: 6},
		}
		if wide {
			cols = append(cols,
				table.Column{Title: "IMAGES", Width: 40},
				table.Column{Title: "SELECTOR", Width: 30})
		}
		return cols
	case viewServices:
		name := width - 16 - 14 - 16 - 16 - 18 - 6 - 8
		if wide {
			name -= 34
		}
		cols := []table.Column{
			{Title: "NAMESPACE", Width: 16},
			{Title: "NAME", Width: clamp(name, 16, 40)},
			{Title: "TYPE", Width: 14},
			{Title: "CLUSTER-IP", Width: 16},
			{Title: "EXTERNAL-IP", Width: 16},
			{Title: "PORT(S)", Width: 18},
			{Title: "AGE", Width: 6},
		}
		if wide {
			cols = append(cols, table.Column{Title: "SELECTOR", Width: 34})
		}
		return cols
	case viewIngresses:
		return []table.Column{
			{Title: "NAMESPACE", Width: 16},
			{Title: "NAME", Width: clamp(width-96, 16, 40)},
			{Title: "CLASS", Width: 12},
			{Title: "HOSTS", Width: 30},
			{Title: "ADDRESS", Width: 16},
			{Title: "PORTS", Width: 8},
			{Title: "AGE", Width: 6},
		}
	case viewDoctor:
		return []table.Column{
			{Title: "CONF", Width: 6},
			{Title: "SIGNATURE", Width: 28},
			{Title: "FINDING", Width: clamp(width-40, 30, 90)},
		}
	case viewVM:
		return []table.Column{
			{Title: "NODE", Width: clamp(width-64, 14, 30)},
			{Title: "ROLE", Width: 8},
			{Title: "ADDRESS", Width: 22},
			{Title: "SSH", Width: 12},
			{Title: "K3S/KUBELET", Width: 18},
		}
	case viewCtx:
		return []table.Column{
			{Title: "", Width: 2},
			{Title: "CONTEXT", Width: clamp(width-40, 16, 40)},
			{Title: "NODES", Width: 7},
			{Title: "SERVER", Width: 24},
		}
	}
	return nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// newViewport builds the scrollable pane used for logs and describe output.
func newViewport(width, height int, body string) viewport.Model {
	if width < 20 {
		width = 20
	}
	if height < 3 {
		height = 3
	}
	vp := viewport.New(width, height)
	vp.SetContent(body)
	return vp
}

// --- port forwards ---

// forward is one active tunnel: kubectl running on the node, plus the SSH
// tunnel that makes its port reachable here.
type forward struct {
	Namespace  string
	Target     string
	RemotePort int
	LocalPort  int
	pf         *kube.PortForward
	tunnel     *ssh.Tunnel
}

func (f forward) String() string {
	return fmt.Sprintf("localhost:%d → %s/%s:%d", f.LocalPort, f.Namespace, f.Target, f.RemotePort)
}

// forwardsMsg carries the updated forward list back to the model.
type forwardsMsg struct {
	forwards []forward
	err      error
}

// startForward opens a port-forward on the node and tunnels it here.
//
// Both halves are needed: kubectl binds on the cluster node, so without the
// tunnel the port is open there and not on the operator's machine.
func startForward(client *ssh.Client, ns, target string, remotePort int, existing []forward) tea.Cmd {
	return func() tea.Msg {
		if client == nil {
			return forwardsMsg{forwards: existing, err: fmt.Errorf("no server connection")}
		}
		// A node-side port distinct from the local one, so several forwards
		// can coexist and neither side collides with something already bound.
		nodePort := 39000 + len(existing)
		pf, err := kube.StartPortForward(client, ns, target, remotePort, nodePort)
		if err != nil {
			return forwardsMsg{forwards: existing, err: err}
		}
		// :0 lets the OS choose a free local port and report which.
		tunnel, err := client.Forward("127.0.0.1:0", pf.NodeAddr())
		if err != nil {
			pf.Stop()
			return forwardsMsg{forwards: existing, err: err}
		}
		local := 0
		if _, portStr, e := net.SplitHostPort(tunnel.LocalAddr); e == nil {
			local, _ = strconv.Atoi(portStr)
		}
		f := forward{
			Namespace: ns, Target: target, RemotePort: remotePort, LocalPort: local,
			pf: pf, tunnel: tunnel,
		}
		return forwardsMsg{forwards: append(existing, f)}
	}
}

// stopForward tears one down, remote process and tunnel both.
func stopForward(fs []forward, idx int) tea.Cmd {
	return func() tea.Msg {
		if idx < 0 || idx >= len(fs) {
			return forwardsMsg{forwards: fs}
		}
		f := fs[idx]
		if f.tunnel != nil {
			f.tunnel.Close()
		}
		if f.pf != nil {
			f.pf.Stop()
		}
		return forwardsMsg{forwards: append(append([]forward{}, fs[:idx]...), fs[idx+1:]...)}
	}
}

func forwardRows(fs []forward) []table.Row {
	rows := make([]table.Row, 0, len(fs))
	for _, f := range fs {
		rows = append(rows, table.Row{
			fmt.Sprintf("%d", f.LocalPort), f.Namespace, f.Target, fmt.Sprintf("%d", f.RemotePort),
		})
	}
	return rows
}

// --- multi-pod logs ---

type multiLogsMsg struct {
	logs []kube.PodLog
	err  error
}

// fetchMultiLogs tails every pod matching the current filter.
func fetchMultiLogs(exec kube.Executor, ns, filter string) tea.Cmd {
	return func() tea.Msg {
		if exec == nil {
			return multiLogsMsg{err: fmt.Errorf("no server connection")}
		}
		logs, err := kube.LogsForSelector(exec, ns, filter, 60)
		return multiLogsMsg{logs: logs, err: err}
	}
}

// renderMultiLogs interleaves pods with a per-pod prefix, so a line is always
// attributable to the pod that wrote it.
func renderMultiLogs(logs []kube.PodLog, width int) string {
	if len(logs) == 0 {
		return "(no pods matched)"
	}
	var b strings.Builder
	for _, l := range logs {
		name := l.Pod
		if len(name) > 28 {
			name = name[:27] + "…"
		}
		header := podPrefixStyle.Render(fmt.Sprintf("── %s/%s ", l.Namespace, l.Pod))
		b.WriteString(header + "\n")
		if l.Err != nil {
			b.WriteString("   " + errStyle.Render(l.Err.Error()) + "\n")
			continue
		}
		if len(l.Lines) == 0 {
			b.WriteString("   (no output)\n")
			continue
		}
		for _, line := range l.Lines {
			b.WriteString(podPrefixStyle.Render(name+" │ ") + truncate(line, max(20, width-32)) + "\n")
		}
	}
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
