//go:build integration

package kube_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/solutionforest/k3helper/internal/kube"
	"github.com/solutionforest/k3helper/internal/sandbox"
)

// TestPortForwardEndToEnd is the whole feature: kubectl port-forward on the
// node, an SSH tunnel to it, and an HTTP request from this machine. Each half
// is useless alone — kubectl binds on the node, and the tunnel has nothing to
// carry without kubectl.
func TestPortForwardEndToEnd(t *testing.T) {
	server := sandbox.Dial(t, "server")

	pods, err := kube.ListPods(server, "default")
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	var target string
	for _, p := range pods {
		if strings.HasPrefix(p.Name, "pf-demo") && p.Healthy() {
			target = p.Name
		}
	}
	if target == "" {
		t.Skip("no running pf-demo pod to forward to")
	}

	pf, err := kube.StartPortForward(server, "default", "pod/"+target, 80, 39100)
	if err != nil {
		t.Fatalf("StartPortForward: %v", err)
	}
	defer pf.Stop()

	tun, err := server.Forward("127.0.0.1:0", pf.NodeAddr())
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	defer tun.Close()

	_, port, _ := net.SplitHostPort(tun.LocalAddr)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/", port))
	if err != nil {
		t.Fatalf("GET through the tunnel: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "nginx") {
		t.Errorf("body does not look like nginx: %q", string(body))
	}
	t.Logf("reached %s through localhost:%s — %d bytes", target, port, len(body))
}

// Multi-pod log tailing must keep each pod's output separate and attributable.
func TestLogsForSelectorLive(t *testing.T) {
	server := sandbox.Dial(t, "server")
	logs, err := kube.LogsForSelector(server, "kube-system", "coredns", 20)
	if err != nil {
		t.Fatalf("LogsForSelector: %v", err)
	}
	if len(logs) == 0 {
		t.Skip("no coredns pods on this cluster")
	}
	for _, l := range logs {
		if l.Namespace != "kube-system" {
			t.Errorf("namespace scoping leaked: %s/%s", l.Namespace, l.Pod)
		}
		if !strings.Contains(l.Pod, "coredns") {
			t.Errorf("selector matched an unrelated pod: %s", l.Pod)
		}
	}
	t.Logf("tailed %d pod(s)", len(logs))
}
