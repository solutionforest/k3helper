// Package tui is the interactive dashboard: k9s-style navigation with
// k3helper's doctor/check/deploy capabilities.
package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/solutionforest/k3helper/internal/check"
	"github.com/solutionforest/k3helper/internal/config"
	"github.com/solutionforest/k3helper/internal/kube"
	"github.com/solutionforest/k3helper/internal/kyaml"
	"github.com/solutionforest/k3helper/internal/ssh"
	"github.com/solutionforest/k3helper/internal/troubleshoot"
	"github.com/solutionforest/k3helper/internal/vm"
)

// refreshInterval is how often the active view reloads itself.
const refreshInterval = 5 * time.Second

// tickMsg drives periodic refresh.
type tickMsg time.Time

// checkDoneMsg carries completed check results and the host samples taken in
// the same pass, so the sparklines and the check verdicts describe the same
// moment.
type checkDoneMsg struct {
	nodeResults map[string][]check.Result
	samples     map[string]hostSample
}

// serverReadyMsg carries the connection used for cluster queries.
type serverReadyMsg struct {
	client *ssh.Client
	err    error
}

// Model is the root TUI model.
type Model struct {
	targets *config.Targets
	// file and targetsPath back the :ctx switcher. Nil file means the targets
	// were handed in directly (tests), and :ctx has nothing to switch between.
	file        *config.File
	targetsPath string

	width   int
	height  int
	spinner spinner.Model
	prog    progress.Model
	loading bool
	// results per node name (dashboard)
	results map[string][]check.Result
	// hist holds the CPU/memory samples the dashboard sparklines draw.
	hist *history
	// view is what is currently on screen
	view view
	err  error
	// status is a transient one-line notice (a file saved, a context switched).
	status string

	// server is the connection cluster queries run through. Nil until the
	// dial completes, or when it failed.
	server    *ssh.Client
	serverErr error

	// resource browser state
	pods      []kube.Pod
	nodes     []kube.Node
	events    []kube.Event
	workloads []kube.Workload
	services  []kube.Service
	ingresses []kube.Ingress
	table     table.Model
	namespace string // "" = all namespaces

	// doctor state
	diagnoses   []troubleshoot.Diagnosis
	unreachable []troubleshoot.UnreachableNode
	probeErrors map[string]string

	// vm state
	vmNodes   []vmNode
	boot      *bootstrapStream
	bootLines []string

	// studio state
	genYAML    string
	genParams  kyaml.GenParams
	deployPath string
	deployFrac float64

	// text pane (logs / describe / multi-pod logs / xray / gen / deploy)
	viewport  viewport.Model
	textTitle string

	// active port forwards
	forwards []forward

	// command bar and filter
	input      textinput.Model
	inputMode  inputMode
	filter     string
	returnView view

	// table presentation
	sortBy     sortKey
	sortRev    bool
	wide       bool
	faultsOnly bool
}

// inputMode says what the text input at the bottom is collecting.
type inputMode int

const (
	inputNone inputMode = iota
	inputCommand
	inputFilter
)

// New creates the TUI model from a targets file.
func New(targets *config.Targets) Model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 120
	return Model{
		targets: targets,
		spinner: s,
		prog:    progress.New(progress.WithDefaultGradient()),
		loading: true,
		results: map[string][]check.Result{},
		hist:    newHistory(),
		view:    viewDashboard,
		input:   in,
		width:   100,
		height:  30,
	}
}

// WithFile attaches the whole targets file so `:ctx` can switch clusters.
func (m Model) WithFile(f *config.File, path string) Model {
	m.file, m.targetsPath = f, path
	return m
}

// NewProgram starts the interactive program (separated for testability).
func NewProgram(m Model) (*tea.Program, error) {
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return p, err
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, doChecks(m.targets), dialServer(m.targets), tickEvery())
}

func tickEvery() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// dialServer opens the connection cluster queries run through.
func dialServer(targets *config.Targets) tea.Cmd {
	return func() tea.Msg {
		srv, err := targets.Server()
		if err != nil {
			return serverReadyMsg{err: err}
		}
		c, err := ssh.Dial(srv.SSH())
		return serverReadyMsg{client: c, err: err}
	}
}

func doChecks(targets *config.Targets) tea.Cmd {
	return func() tea.Msg {
		results := map[string][]check.Result{}
		samples := map[string]hostSample{}
		for _, node := range targets.Nodes {
			client, err := ssh.Dial(node.SSH())
			if err != nil {
				results[node.Name] = []check.Result{{
					ID: "ssh.connect", Category: "host", Name: "SSH connectivity",
					Status: check.Fail, Summary: fmt.Sprintf("unreachable: %v", err),
				}}
				continue
			}
			runner := check.NewRunner(
				check.DiskUsageCheck{},
				check.MemoryCheck{},
				check.SwapCheck{},
				check.CgroupCheck{},
				check.ServiceCheck{Role: node.Role},
			)
			results[node.Name] = runner.RunAll(check.Context{Exec: execAdapter{client}, Node: node.Name})
			samples[node.Name] = sampleHost(client)
			client.Close()
		}
		return checkDoneMsg{nodeResults: results, samples: samples}
	}
}

type execAdapter struct{ c *ssh.Client }

func (a execAdapter) Run(cmd string) (string, int, error) { return a.c.Run(cmd) }

// selectorAndFilter splits the filter bar into a label selector the API server
// evaluates and a plain filter applied to the rows.
//
// `-l app=web` is kubectl's own syntax, and an operator who types it expects
// kubectl's behaviour: matching it as a substring against row text would
// quietly return nothing.
func (m Model) selectorAndFilter() (selector, filter string) {
	f := strings.TrimSpace(m.filter)
	if rest, ok := strings.CutPrefix(f, "-l "); ok {
		return strings.TrimSpace(rest), ""
	}
	return "", f
}

// rowOptions is the current presentation state, for the row builders.
func (m Model) rowOptions() rowOpts {
	_, filter := m.selectorAndFilter()
	return newRowOpts(filter, m.wide, m.faultsOnly)
}

// refresh reloads whatever the active view shows.
func (m Model) refresh() tea.Cmd {
	selector, _ := m.selectorAndFilter()
	switch m.view {
	case viewDashboard:
		return doChecks(m.targets)
	case viewPods, viewNodes, viewEvents, viewDeployments, viewStatefulSets,
		viewDaemonSets, viewServices, viewIngresses:
		return fetchResources(m.server, m.view, m.namespace, selector)
	case viewMultiLogs:
		return fetchMultiLogs(m.server, m.namespace, m.filter)
	case viewDoctor:
		return runDoctor(m.targets)
	case viewXray:
		return fetchXray(m.server, m.namespace)
	case viewVM:
		return probeTargets(m.targets)
	}
	return nil // logs/describe/ports/gen/deploy are point-in-time or locally held
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.prog.Width = clamp(m.width-40, 20, 60)
		m.applySize()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tickMsg:
		// Reload the active view, then schedule the next tick. The previous
		// implementation only rescheduled, so nothing ever refreshed.
		return m, tea.Batch(m.refresh(), tickEvery())

	case serverReadyMsg:
		m.server, m.serverErr = msg.client, msg.err
		if msg.err == nil && m.view != viewDashboard {
			return m, m.refresh()
		}
		return m, nil

	case checkDoneMsg:
		m.results = msg.nodeResults
		for node, s := range msg.samples {
			m.hist.push(node, s)
		}
		m.loading = false
		return m, nil

	case resourcesMsg:
		return m.handleResources(msg)

	case doctorMsg:
		m.loading = false
		m.err = msg.err
		if msg.err == nil {
			m.diagnoses, m.unreachable, m.probeErrors = msg.diagnoses, msg.unreachable, msg.probeErrors
			m.view = viewDoctor
			m.rebuildTable()
		}
		return m, nil

	case vmStatusMsg:
		m.loading = false
		m.vmNodes = msg.nodes
		if m.view == viewVM {
			m.rebuildTable()
		}
		return m, nil

	case vmLogMsg:
		return m.handleBootstrapLine(msg)

	case xrayMsg:
		m.loading = false
		m.err = msg.err
		if msg.err == nil {
			m.view = viewXray
			m.textTitle = "ownership graph"
			m.viewport = newViewport(m.width-2, m.bodyHeight(), msg.body)
		}
		return m, nil

	case genMsg:
		m.loading = false
		m.err = msg.err
		if msg.err == nil {
			m.view = viewGen
			m.genYAML, m.genParams = msg.yaml, msg.params
			m.textTitle = "YAML studio — " + msg.params.Kind + "/" + msg.params.Name
			m.viewport = newViewport(m.width-2, m.bodyHeight(), msg.body)
		}
		return m, nil

	case savedMsg:
		m.err = msg.err
		if msg.err == nil {
			m.status = "saved " + msg.path
		}
		return m, nil

	case deployMsg:
		m.loading = false
		m.err = msg.err
		m.deployPath = msg.file
		if msg.err == nil {
			m.view = viewDeploy
			m.deployFrac = 0
			if msg.applied {
				m.deployFrac = rolloutFraction(msg.result)
			}
			m.textTitle = "deploy — " + msg.file
			m.viewport = newViewport(m.width-2, m.bodyHeight(), msg.body)
		}
		return m, nil

	case forwardsMsg:
		m.loading = false
		m.err = msg.err
		m.forwards = msg.forwards
		if m.view == viewPorts {
			m.rebuildTable()
		}
		return m, nil

	case multiLogsMsg:
		m.loading = false
		m.err = msg.err
		if msg.err == nil {
			m.view = viewMultiLogs
			m.textTitle = fmt.Sprintf("logs from %d pod(s)", len(msg.logs))
			m.viewport = newViewport(m.width-2, m.bodyHeight(), renderMultiLogs(msg.logs, m.width))
		}
		return m, nil

	case textMsg:
		m.loading = false
		m.err = msg.err
		if msg.err == nil {
			m.view = msg.view
			m.textTitle = msg.title
			m.viewport = newViewport(m.width-2, m.bodyHeight(), msg.body)
		}
		return m, nil

	case progress.FrameMsg:
		p, cmd := m.prog.Update(msg)
		m.prog = p.(progress.Model)
		return m, cmd

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

// handleResources takes a value receiver so Update always returns the same
// concrete type: a pointer here would make tea.Model sometimes *Model and
// sometimes Model, and callers type-asserting on it would panic.
func (m Model) handleResources(msg resourcesMsg) (tea.Model, tea.Cmd) {
	m.loading = false
	m.err = msg.err
	if msg.err != nil {
		return m, nil
	}
	m.view = msg.view
	switch msg.view {
	case viewPods:
		m.pods = msg.pods
	case viewNodes:
		m.nodes = msg.nodes
	case viewEvents:
		m.events = msg.events
	case viewDeployments, viewStatefulSets, viewDaemonSets:
		m.workloads = msg.workloads
	case viewServices:
		m.services = msg.services
	case viewIngresses:
		m.ingresses = msg.ingresses
	}
	m.rebuildTable()
	return m, nil
}

// handleBootstrapLine appends streamed install output and keeps reading until
// the install goroutine reports it is done.
func (m Model) handleBootstrapLine(msg vmLogMsg) (tea.Model, tea.Cmd) {
	if msg.line != "" {
		m.bootLines = append(m.bootLines, msg.line)
	}
	if msg.done {
		m.loading = false
		m.err = msg.err
		if msg.err == nil {
			m.bootLines = append(m.bootLines, "✓ bootstrap complete")
		}
		m.boot = nil
		m.viewport = newViewport(m.width-2, m.bodyHeight(), strings.Join(m.bootLines, "\n"))
		// The targets view now describes a cluster that exists.
		return m, probeTargets(m.targets)
	}
	m.viewport = newViewport(m.width-2, m.bodyHeight(), strings.Join(m.bootLines, "\n"))
	m.viewport.GotoBottom()
	return m, waitForBootstrapLine(m.boot)
}

// rebuildTable regenerates rows for the active view, preserving the cursor so
// an auto-refresh does not yank the selection out from under the operator.
func (m *Model) rebuildTable() {
	o := m.rowOptions()
	var rows []table.Row
	switch m.view {
	case viewPods:
		rows = podRows(m.pods, o)
	case viewNodes:
		rows = nodeRows(m.nodes, o)
	case viewEvents:
		rows = eventRows(m.events, o)
	case viewDeployments, viewStatefulSets, viewDaemonSets:
		rows = workloadRows(m.workloads, o)
	case viewServices:
		rows = serviceRows(m.services, o)
	case viewIngresses:
		rows = ingressRows(m.ingresses, o)
	case viewDoctor:
		rows = doctorRows(m.diagnoses, o)
	case viewVM:
		rows = vmRows(m.vmNodes, o)
	case viewCtx:
		rows = ctxRows(m.file, m.targets.Cluster, o)
	case viewPorts:
		rows = forwardRows(m.forwards)
	default:
		return
	}
	sortRows(rows, m.view, m.sortBy, m.sortRev)
	cursor := m.table.Cursor()
	m.table = newTable(columnsFor(m.view, m.width, m.wide), rows, m.bodyHeight())
	if cursor > 0 && cursor < len(rows) {
		m.table.SetCursor(cursor)
	}
}

// bodyHeight is the space left for the table or text pane after the header,
// help line and command bar.
func (m Model) bodyHeight() int {
	h := m.height - 7
	if h < 3 {
		return 3
	}
	return h
}

func (m *Model) applySize() {
	if m.view.isText() {
		m.viewport.Width = m.width - 2
		m.viewport.Height = m.bodyHeight()
		return
	}
	m.rebuildTable()
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The command bar and filter capture every key except submit/cancel.
	if m.inputMode != inputNone {
		switch msg.String() {
		case "esc":
			m.inputMode = inputNone
			m.input.Blur()
			m.input.SetValue("")
			return m, nil
		case "enter":
			value := m.input.Value()
			mode := m.inputMode
			m.inputMode = inputNone
			m.input.Blur()
			m.input.SetValue("")
			if mode == inputCommand {
				return m.runCommand(value)
			}
			m.filter = value
			switch {
			case m.view == viewMultiLogs:
				m.loading = true
				return m, fetchMultiLogs(m.server, m.namespace, m.filter)
			case strings.HasPrefix(strings.TrimSpace(value), "-l "):
				// A label selector is evaluated by the API server, so it needs
				// a refetch rather than a re-filter of the rows in hand.
				m.loading = true
				return m, m.refresh()
			}
			m.rebuildTable()
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		if m.inputMode == inputFilter {
			// live filtering as the operator types, except for a label
			// selector, which only takes effect on enter
			m.filter = m.input.Value()
			if !strings.HasPrefix(strings.TrimSpace(m.filter), "-l") {
				m.rebuildTable()
			}
		}
		return m, cmd
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit

	case ":":
		m.inputMode = inputCommand
		m.input.Placeholder = "po | no | dp | svc | ing | sts | ds | ev | doctor | xray | vm | ctx | gen <kind> <name> | deploy <file>"
		m.input.Focus()
		return m, nil

	case "/":
		if m.view.isTable() {
			m.inputMode = inputFilter
			m.input.Placeholder = "regex, or -l app=web"
			m.input.SetValue(m.filter)
			m.input.Focus()
		}
		return m, nil

	case "esc":
		// Step back out of a drill-down, or clear an active filter.
		if m.view.isText() {
			m.view = m.returnView
			if m.returnView == "" {
				m.view = viewPods
			}
			m.err = nil
			m.rebuildTable()
			return m, m.refresh()
		}
		if m.filter != "" {
			m.filter = ""
			m.rebuildTable()
			return m, m.refresh()
		}
		return m, nil

	case "r":
		m.loading = true
		m.status = ""
		return m, m.refresh()

	case "ctrl+w":
		// wide mode: extra columns, k9s-style
		if m.view.isTable() {
			m.wide = !m.wide
			m.rebuildTable()
		}
		return m, nil

	case "ctrl+z":
		// faults only: hide everything healthy
		if m.view.isTable() {
			m.faultsOnly = !m.faultsOnly
			m.rebuildTable()
		}
		return m, nil

	case "N", "A", "S":
		if m.view.isTable() {
			key := map[string]sortKey{"N": sortName, "A": sortAge, "S": sortStatus}[msg.String()]
			if m.sortBy == key {
				m.sortRev = !m.sortRev
			} else {
				m.sortBy, m.sortRev = key, false
			}
			m.rebuildTable()
		}
		return m, nil

	case "l", "enter":
		return m.handleOpen()

	case "f":
		// forward a port from the selected pod or service
		switch m.view {
		case viewPods:
			if ns, name, ok := m.selectedPod(); ok {
				port := m.podPort(name)
				m.loading = true
				return m, startForward(m.server, ns, "pod/"+name, port, m.forwards)
			}
		case viewServices:
			if row := m.table.SelectedRow(); len(row) > 5 {
				m.loading = true
				return m, startForward(m.server, row[0], "svc/"+row[1], firstPort(row[5]), m.forwards)
			}
		}
		return m, nil

	case "x":
		// stop the selected forward
		if m.view == viewPorts {
			return m, stopForward(m.forwards, m.table.Cursor())
		}
		return m, nil

	case "d":
		return m.handleDescribe()

	case "s":
		// save the generated manifest
		if m.view == viewGen && m.genYAML != "" {
			return m, saveGen(m.genYAML, m.genParams)
		}
		return m, nil

	case "a":
		// apply the manifest whose dry-run is on screen
		if m.view == viewDeploy && m.deployPath != "" {
			m.loading = true
			return m, applyDeploy(m.server, m.deployPath, m.namespace)
		}
		return m, nil

	case "b":
		// bootstrap every target from the :vm view
		if m.view == viewVM {
			return m.startBootstrapFlow()
		}
		return m, nil
	}

	// Remaining keys drive the active widget.
	var cmd tea.Cmd
	if m.view.isText() {
		m.viewport, cmd = m.viewport.Update(msg)
	} else if m.view != viewDashboard {
		m.table, cmd = m.table.Update(msg)
	}
	return m, cmd
}

// handleOpen is enter/l: what "open the selected row" means per view.
func (m Model) handleOpen() (tea.Model, tea.Cmd) {
	switch {
	case m.view == viewPods:
		if ns, name, ok := m.selectedPod(); ok {
			m.returnView, m.loading = m.view, true
			return m, fetchLogs(m.server, ns, name)
		}
	case m.view.isWorkload():
		// Drill into the pods this workload owns, using its own selector so
		// the list is exact rather than a name-prefix guess.
		if w, ok := m.selectedWorkload(); ok {
			m.returnView = m.view
			m.view = viewPods
			m.namespace = w.Namespace
			m.filter = "-l " + w.Selector
			m.loading = true
			return m, fetchResources(m.server, viewPods, w.Namespace, w.Selector)
		}
	case m.view == viewServices:
		if row := m.table.SelectedRow(); len(row) > 1 {
			for _, s := range m.services {
				if s.Namespace == row[0] && s.Name == row[1] && s.Selector != "" {
					m.returnView = m.view
					m.view = viewPods
					m.namespace = s.Namespace
					m.filter = "-l " + s.Selector
					m.loading = true
					return m, fetchResources(m.server, viewPods, s.Namespace, s.Selector)
				}
			}
		}
	case m.view == viewDoctor:
		if d, ok := m.selectedDiagnosis(); ok {
			m.returnView = m.view
			m.view = viewDescribe
			m.textTitle = "finding — " + d.SignatureID
			m.viewport = newViewport(m.width-2, m.bodyHeight(), doctorDetail(d))
		}
	case m.view == viewCtx:
		return m.switchContext()
	}
	return m, nil
}

// handleDescribe is `d`: describe whatever kind the active view lists.
func (m Model) handleDescribe() (tea.Model, tea.Cmd) {
	row := m.table.SelectedRow()
	switch m.view {
	case viewPods:
		if ns, name, ok := m.selectedPod(); ok {
			m.returnView, m.loading = m.view, true
			return m, fetchDescribe(m.server, "pod", ns, name)
		}
	case viewNodes:
		if len(row) > 0 {
			m.returnView, m.loading = m.view, true
			return m, fetchDescribe(m.server, "node", "", row[0])
		}
	case viewDeployments, viewStatefulSets, viewDaemonSets, viewServices, viewIngresses:
		if len(row) > 1 {
			m.returnView, m.loading = m.view, true
			return m, fetchDescribe(m.server, describeKind(m.view), row[0], row[1])
		}
	}
	return m, nil
}

// describeKind maps a view to the singular kind kubectl describe wants.
func describeKind(v view) string {
	switch v {
	case viewDeployments:
		return "deployment"
	case viewStatefulSets:
		return "statefulset"
	case viewDaemonSets:
		return "daemonset"
	case viewServices:
		return "service"
	case viewIngresses:
		return "ingress"
	}
	return "pod"
}

// startBootstrapFlow installs k3s on every target, streaming output into the
// text pane.
func (m Model) startBootstrapFlow() (tea.Model, tea.Cmd) {
	if m.boot != nil {
		return m, nil // already running
	}
	m.bootLines = []string{fmt.Sprintf("bootstrapping %d node(s) in %s…", len(m.targets.Nodes), m.targets.Cluster)}
	stream, cmd := startBootstrap(m.targets, vm.Options{})
	m.boot = stream
	m.loading = true
	m.view = viewDescribe
	m.returnView = viewVM
	m.textTitle = "vm bootstrap"
	m.viewport = newViewport(m.width-2, m.bodyHeight(), strings.Join(m.bootLines, "\n"))
	return m, cmd
}

// switchContext moves the whole dashboard to another cluster in the file.
func (m Model) switchContext() (tea.Model, tea.Cmd) {
	row := m.table.SelectedRow()
	if m.file == nil || len(row) < 2 {
		return m, nil
	}
	t, err := m.file.Select(row[1])
	if err != nil {
		m.err = err
		return m, nil
	}
	// Everything held in the model describes the old cluster: keeping any of
	// it would show one cluster's pods under another's name.
	if m.server != nil {
		m.server.Close()
	}
	for _, f := range m.forwards {
		if f.tunnel != nil {
			f.tunnel.Close()
		}
		if f.pf != nil {
			f.pf.Stop()
		}
	}
	m.targets = t
	m.server, m.serverErr = nil, nil
	m.pods, m.nodes, m.events = nil, nil, nil
	m.workloads, m.services, m.ingresses = nil, nil, nil
	m.diagnoses, m.unreachable, m.probeErrors = nil, nil, nil
	m.vmNodes, m.forwards = nil, nil
	m.results = map[string][]check.Result{}
	m.hist = newHistory()
	m.filter, m.namespace = "", ""
	m.status = "switched to " + t.Cluster
	m.view = viewDashboard
	m.loading = true
	return m, tea.Batch(dialServer(t), doChecks(t))
}

// selectedPod returns the namespace and name of the highlighted pod row.
func (m Model) selectedPod() (string, string, bool) {
	row := m.table.SelectedRow()
	if len(row) < 2 {
		return "", "", false
	}
	return row[0], row[1], true
}

// selectedWorkload finds the workload behind the highlighted row.
func (m Model) selectedWorkload() (kube.Workload, bool) {
	row := m.table.SelectedRow()
	if len(row) < 2 {
		return kube.Workload{}, false
	}
	for _, w := range m.workloads {
		if w.Namespace == row[0] && w.Name == row[1] {
			return w, w.Selector != ""
		}
	}
	return kube.Workload{}, false
}

// selectedDiagnosis finds the finding behind the highlighted doctor row.
func (m Model) selectedDiagnosis() (troubleshoot.Diagnosis, bool) {
	row := m.table.SelectedRow()
	if len(row) < 2 {
		return troubleshoot.Diagnosis{}, false
	}
	for _, d := range m.diagnoses {
		if d.SignatureID == row[1] {
			return d, true
		}
	}
	return troubleshoot.Diagnosis{}, false
}

// firstPort reads a service's first port out of its "80/TCP,443:30443/TCP"
// column, for the port-forward key.
func firstPort(ports string) int {
	first, _, _ := strings.Cut(ports, ",")
	numeric, _, _ := strings.Cut(first, "/")
	numeric, _, _ = strings.Cut(numeric, ":")
	if p, err := strconv.Atoi(strings.TrimSpace(numeric)); err == nil && p > 0 {
		return p
	}
	return 80
}

// runCommand handles a `:` command-bar entry.
func (m Model) runCommand(cmd string) (tea.Model, tea.Cmd) {
	cmd = strings.TrimSpace(cmd)
	m.status = ""

	// `:ns <name>` scopes resource views to one namespace ("all" clears it).
	if rest, ok := strings.CutPrefix(cmd, "ns "); ok {
		rest = strings.TrimSpace(rest)
		if rest == "all" || rest == "*" {
			rest = ""
		}
		m.namespace = rest
		m.loading = true
		return m, m.refresh()
	}
	// `:gen <kind> <name> ...` opens the YAML studio.
	if rest, ok := strings.CutPrefix(cmd, "gen"); ok && (rest == "" || strings.HasPrefix(rest, " ")) {
		m.err = nil
		m.returnView = m.view
		m.loading = true
		return m, genFlow(strings.TrimSpace(rest))
	}
	// `:deploy <file>` is the apply flow; bare `:deploy` is the deployments
	// list, resolved below like any other resource alias.
	if rest, ok := strings.CutPrefix(cmd, "deploy "); ok && strings.TrimSpace(rest) != "" {
		m.err = nil
		m.returnView = m.view
		m.loading = true
		return m, dryRunDeploy(m.server, strings.TrimSpace(rest), m.namespace)
	}
	// `:theme <name>` switches skin without restarting.
	if rest, ok := strings.CutPrefix(cmd, "theme "); ok {
		if err := SetTheme(strings.TrimSpace(rest)); err != nil {
			m.err = err
			return m, nil
		}
		m.err = nil
		m.status = "theme: " + activeTheme.Name
		m.rebuildTable()
		return m, nil
	}

	v, ok := resolveView(cmd)
	if !ok {
		m.err = fmt.Errorf("unknown command %q — try po, no, dp, svc, ing, sts, ds, ev, "+
			"doctor, xray, vm, ctx, logs, ports, dashboard, ns <name>, gen <kind> <name>, "+
			"deploy <file>, theme <name>", cmd)
		return m, nil
	}
	m.err = nil
	m.filter = ""
	m.sortBy, m.sortRev = sortNone, false
	switch v {
	case viewDashboard:
		m.view = v
		m.loading = true
		return m, doChecks(m.targets)
	case viewPorts:
		m.view = v
		m.rebuildTable()
		return m, nil
	case viewCtx:
		if m.file == nil {
			m.err = fmt.Errorf("no targets file loaded — :ctx needs a multi-cluster file")
			return m, nil
		}
		m.view = v
		m.rebuildTable()
		return m, nil
	case viewMultiLogs:
		m.returnView = viewPods
		m.loading = true
		return m, fetchMultiLogs(m.server, m.namespace, m.filter)
	case viewXray:
		m.returnView = m.view
		m.loading = true
		return m, fetchXray(m.server, m.namespace)
	case viewDoctor:
		m.view = v
		m.loading = true
		return m, runDoctor(m.targets)
	case viewVM:
		m.view = v
		m.loading = true
		return m, probeTargets(m.targets)
	}
	m.view = v
	m.loading = true
	return m, fetchResources(m.server, v, m.namespace, "")
}

// View implements tea.Model.
func (m Model) View() string {
	var b strings.Builder
	b.WriteString(m.header())

	switch {
	case m.loading && m.view == viewDashboard && len(m.results) == 0:
		b.WriteString("  " + m.spinner.View() + " running checks...\n")
	case m.view == viewDashboard:
		b.WriteString(m.dashboardBody())
	case m.view.isText():
		b.WriteString(m.viewport.View() + "\n")
		if m.view == viewDeploy && m.deployFrac > 0 {
			b.WriteString("  rollout " + m.prog.ViewAs(m.deployFrac) + "\n")
		}
		if m.boot != nil {
			b.WriteString("  " + m.spinner.View() + " " +
				m.prog.ViewAs(bootstrapFraction(m.bootLines, len(m.targets.Nodes))) + "\n")
		}
	case m.view == viewPorts && len(m.forwards) == 0:
		b.WriteString(helpStyle.Render(
			"  no active port forwards.\n\n"+
				"  Open one from the pod list: :pods, select a pod, press f.\n"+
				"  kubectl binds on the cluster node, so k3helper also opens an\n"+
				"  SSH tunnel — the local address shown is reachable from here.\n") + "\n")
	case m.view == viewDoctor && len(m.diagnoses) == 0 && !m.loading:
		b.WriteString(statusOKStyle.Render("  ✓ no issues detected — cluster looks healthy") + "\n")
		for _, line := range probeErrorLines(m.probeErrors) {
			b.WriteString(statusWarnStyle.Render("  ! "+line) + "\n")
		}
	case m.view == viewVM:
		b.WriteString(m.table.View() + "\n")
		b.WriteString(helpStyle.Render("  b: bootstrap every target with k3s") + "\n")
	default:
		b.WriteString(m.table.View() + "\n")
	}

	if m.status != "" {
		b.WriteString(statusOKStyle.Render("  "+m.status) + "\n")
	}
	if m.err != nil {
		b.WriteString(errStyle.Render("  ! "+m.err.Error()) + "\n")
	}
	b.WriteString(m.footer())
	return b.String()
}

// header is the always-visible status bar: context, namespace, health score,
// fault count, and the toggles currently in effect.
func (m Model) header() string {
	title := "☸ k3helper — " + m.targets.Cluster
	var parts []string
	parts = append(parts, string(m.view))
	if m.namespace != "" {
		parts = append(parts, "ns:"+m.namespace)
	} else if m.view.isTable() && m.view != viewNodes && m.view != viewVM && m.view != viewCtx {
		parts = append(parts, "all namespaces")
	}
	if m.filter != "" {
		parts = append(parts, "/"+m.filter)
	}
	if m.sortBy != sortNone {
		arrow := "↑"
		if m.sortRev {
			arrow = "↓"
		}
		parts = append(parts, "sort:"+m.sortBy.String()+arrow)
	}
	if m.wide {
		parts = append(parts, "wide")
	}
	if m.faultsOnly {
		parts = append(parts, "faults")
	}
	if m.view.isText() && m.textTitle != "" {
		parts = []string{m.textTitle}
	}

	score, have := m.healthScore()
	scoreText := helpStyle.Render("score n/a")
	if have {
		style := statusOKStyle
		switch {
		case score < 60:
			style = statusFailStyle
		case score < 90:
			style = statusWarnStyle
		}
		scoreText = style.Render(fmt.Sprintf("score %d%%", score))
	}
	faults := helpStyle.Render("doctor: not run")
	if m.diagnoses != nil || m.view == viewDoctor {
		faults = doctorSummary(m.diagnoses)
	}

	return titleStyle.Render(title) + "  " + helpStyle.Render(strings.Join(parts, " · ")) +
		"  " + scoreText + "  " + faults + "\n\n"
}

// healthScore is the share of check results that are OK, which is what the
// dashboard ring and the status bar both report.
func (m Model) healthScore() (int, bool) {
	ok, total := 0, 0
	for _, rs := range m.results {
		for _, r := range rs {
			total++
			if r.Status == check.OK {
				ok++
			}
		}
	}
	if total == 0 {
		return 0, false
	}
	return ok * 100 / total, true
}

func (m Model) footer() string {
	if m.inputMode == inputCommand {
		return "\n:" + m.input.View()
	}
	if m.inputMode == inputFilter {
		return "\n/" + m.input.View()
	}
	var keys string
	switch {
	case m.view == viewDashboard:
		keys = ": command  r: refresh  q: quit"
	case m.view == viewPods:
		keys = "↑↓: move  enter/l: logs  d: describe  f: port-forward  /: filter  ^w: wide  ^z: faults  NAS: sort  q: quit"
	case m.view == viewNodes:
		keys = "↑↓: move  d: describe  /: filter  ^w: wide  ^z: faults  NAS: sort  :: command  q: quit"
	case m.view == viewEvents:
		keys = "↑↓: move  /: filter  ^z: warnings only  NAS: sort  :: command  q: quit"
	case m.view.isWorkload():
		keys = "↑↓: move  enter: its pods  d: describe  /: filter  ^w: wide  ^z: faults  NAS: sort  q: quit"
	case m.view == viewServices:
		keys = "↑↓: move  enter: its pods  d: describe  f: port-forward  /: filter  ^w: wide  q: quit"
	case m.view == viewIngresses:
		keys = "↑↓: move  d: describe  /: filter  ^z: pending only  NAS: sort  :: command  q: quit"
	case m.view == viewDoctor:
		keys = "↑↓: move  enter: evidence + fix  r: re-run  /: filter  S: by confidence  q: quit"
	case m.view == viewVM:
		keys = "↑↓: move  b: bootstrap  r: re-probe  /: filter  :: command  q: quit"
	case m.view == viewCtx:
		keys = "↑↓: move  enter: switch cluster  :: command  q: quit"
	case m.view == viewPorts:
		keys = "↑↓: move  x: stop forward  :pods then f: add one  :: command  q: quit"
	case m.view == viewMultiLogs:
		keys = "↑↓/pgup/pgdn: scroll  /: filter pods  r: refresh  esc: back  q: quit"
	case m.view == viewGen:
		keys = "s: save  ↑↓: scroll  esc: back  q: quit"
	case m.view == viewDeploy:
		keys = "a: apply  ↑↓: scroll  esc: back  q: quit"
	default:
		keys = "↑↓/pgup/pgdn: scroll  esc: back  q: quit"
	}
	if m.serverErr != nil {
		keys = "cluster unreachable: " + m.serverErr.Error() + "  ·  " + keys
	}
	return "\n" + helpStyle.Render(" "+keys)
}

// dashboardBody renders the per-node check cards with load sparklines.
func (m Model) dashboardBody() string {
	var b strings.Builder
	ok, warn, fail := 0, 0, 0
	for _, rs := range m.results {
		for _, r := range rs {
			switch r.Status {
			case check.OK:
				ok++
			case check.Warn:
				warn++
			case check.Fail:
				fail++
			}
		}
	}
	score, have := m.healthScore()
	if have {
		b.WriteString("  " + healthRing(score) + "  ")
	} else {
		b.WriteString("  ")
	}
	b.WriteString(fmt.Sprintf("%s %d ok   %s %d warn   %s %d fail\n\n",
		statusOKStyle.Render("●"), ok,
		statusWarnStyle.Render("●"), warn,
		statusFailStyle.Render("●"), fail))

	for _, node := range m.targets.Nodes {
		rs := m.results[node.Name]
		var lines []string
		for _, r := range rs {
			style := statusOKStyle
			switch r.Status {
			case check.Warn:
				style = statusWarnStyle
			case check.Fail:
				style = statusFailStyle
			}
			lines = append(lines, fmt.Sprintf("%s %-24s %s", style.Render(r.Status.Icon()), r.Name, r.Summary))
			if r.Status == check.Fail && r.Remediation != "" {
				lines = append(lines, "    "+statusFailStyle.Render("↳ "+r.Remediation))
			}
		}
		if graph := m.nodeGraphs(node.Name); graph != "" {
			lines = append(lines, graph)
		}
		card := fmt.Sprintf("%s (%s)\n%s", node.Name, node.Role, strings.Join(lines, "\n"))
		b.WriteString(paneBorder.Render(card) + "\n")
	}
	return b.String()
}

// nodeGraphs is the CPU/memory sparkline pair for one node's card.
func (m Model) nodeGraphs(node string) string {
	cpu, mem := m.hist.cpuFor(node), m.hist.memFor(node)
	if len(cpu) == 0 && len(mem) == 0 {
		return ""
	}
	cpuNow, _ := latest(cpu)
	memNow, _ := latest(mem)
	return fmt.Sprintf("%s cpu %s %3.0f%%   mem %s %3.0f%%",
		helpStyle.Render("▸"),
		sparkline(cpu, historyLen), cpuNow,
		sparkline(mem, historyLen), memNow)
}

// healthRing is the score as a filled arc — the dashboard's one glanceable
// number.
func healthRing(score int) string {
	const width = 10
	filled := score * width / 100
	style := statusOKStyle
	switch {
	case score < 60:
		style = statusFailStyle
	case score < 90:
		style = statusWarnStyle
	}
	return style.Render(strings.Repeat("◕", filled)+strings.Repeat("◔", width-filled)) +
		style.Render(fmt.Sprintf(" %d%%", score))
}

// podPort guesses the port to forward for a pod: the containerPort it
// declares, falling back to 80. The pod list does not carry ports, so this
// asks the cluster for the one pod being forwarded rather than every pod.
func (m Model) podPort(name string) int {
	if m.server == nil {
		return 80
	}
	out, code, err := m.server.Run(kube.Builder(m.server)(
		"get pod " + name + " -o jsonpath={.spec.containers[0].ports[0].containerPort}"))
	if err != nil || code != 0 {
		return 80
	}
	if p, err := strconv.Atoi(strings.TrimSpace(out)); err == nil && p > 0 {
		return p
	}
	return 80
}
