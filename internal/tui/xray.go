package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/solutionforest/k3helper/internal/kube"
)

// xrayMsg carries a built ownership graph.
type xrayMsg struct {
	body string
	err  error
}

// fetchXray builds the deployment → replicaset → pod tree.
func fetchXray(exec kube.Executor, ns string) tea.Cmd {
	return func() tea.Msg {
		if exec == nil {
			return xrayMsg{err: fmt.Errorf("no server connection")}
		}
		roots, err := kube.OwnerTree(exec, ns)
		if err != nil {
			return xrayMsg{err: err}
		}
		return xrayMsg{body: renderTree(roots)}
	}
}

// renderTree draws the graph with box-drawing connectors, health-coloured.
func renderTree(roots []*kube.TreeNode) string {
	if len(roots) == 0 {
		return "(no workloads)"
	}
	var b strings.Builder
	var ns string
	for _, r := range roots {
		if r.Namespace != ns {
			ns = r.Namespace
			b.WriteString(sectionStyle.Render(ns) + "\n")
		}
		writeTreeNode(&b, r, "", true)
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeTreeNode(b *strings.Builder, n *kube.TreeNode, prefix string, last bool) {
	connector := "├─ "
	childPrefix := prefix + "│  "
	if last {
		connector = "└─ "
		childPrefix = prefix + "   "
	}
	icon := statusFailStyle.Render("✗")
	if n.Healthy {
		icon = statusOKStyle.Render("✓")
	}
	b.WriteString(fmt.Sprintf("%s%s%s %s %s  %s\n",
		prefix, helpStyle.Render(connector), icon,
		helpStyle.Render(strings.ToLower(n.Kind)), n.Name,
		helpStyle.Render(n.Status)))
	for i, c := range n.Children {
		writeTreeNode(b, c, childPrefix, i == len(n.Children)-1)
	}
}
