// Package vm installs k3s on target nodes over SSH (k3sup-style bootstrap).
package vm

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/solutionforest/k3helper/internal/ssh"
)

// Options control the k3s bootstrap.
type Options struct {
	// InstallURL overrides the k3s install script (tests use a local stub).
	InstallURL string
	// Channel: stable, latest, or a fixed version like v1.31.2+k3s1
	Channel string
	// ExtraArgs appended to the install command (e.g. "--disable traefik")
	ServerExtraArgs string
	AgentExtraArgs  string
	// Token for joining agents/servers (generated if empty)
	Token string
	// Progress receives per-node status lines (optional)
	Progress io.Writer
}

func (o *Options) installURL() string {
	if o.InstallURL != "" {
		return o.InstallURL
	}
	return "https://get.k3s.io"
}

func (o Options) channel() string {
	if o.Channel == "" {
		return "stable"
	}
	return o.Channel
}

// Setup installs k3s on the server then joins agents.
// targets is expressed with minimal structural coupling: serverNode + agents.
func Setup(server *ssh.Client, serverNode ssh.Node, agents []struct {
	Node   ssh.Node
	Client *ssh.Client
}, opts Options) error {
	progressf := func(format string, a ...interface{}) {
		if opts.Progress != nil {
			fmt.Fprintf(opts.Progress, format+"\n", a...)
		}
	}

	// 1. install k3s on server
	progressf("[server] installing k3s server (%s channel)...", opts.channel())
	tokenArg := ""
	if opts.Token != "" {
		tokenArg = fmt.Sprintf(" K3S_TOKEN=%s", opts.Token)
	}
	cmd := fmt.Sprintf(
		`curl -sfL %s | %sINSTALL_K3S_CHANNEL=%s%s sh -s - server%s`,
		opts.installURL(), server.SudoPrefix(), opts.channel(), tokenArg, withSpace(opts.ServerExtraArgs),
	)
	if code, err := streamSudo(server, cmd, opts.Progress); err != nil || code != 0 {
		return fmt.Errorf("server install failed (exit %d): %w", code, err)
	}

	// 2. fetch node token from server
	progressf("[server] fetching join token...")
	token, err := serverToken(server)
	if err != nil {
		return fmt.Errorf("fetch token: %w", err)
	}

	// 3. fetch server internal IP for agents to join
	ip, err := serverInternalIP(server)
	if err != nil {
		return fmt.Errorf("fetch server IP: %w", err)
	}

	// 4. join agents
	for _, a := range agents {
		progressf("[%s] installing k3s agent...", a.Node.Host)
		joinCmd := fmt.Sprintf(
			`curl -sfL %s | %sK3S_URL=https://%s:6443 K3S_TOKEN=%s INSTALL_K3S_CHANNEL=%s sh -s - agent%s`,
			opts.installURL(), a.Client.SudoPrefix(), ip, token, opts.channel(), withSpace(opts.AgentExtraArgs),
		)
		if code, err := streamSudo(a.Client, joinCmd, opts.Progress); err != nil || code != 0 {
			return fmt.Errorf("agent %s install failed (exit %d): %w", a.Node.Host, code, err)
		}
	}

	// 5. wait for every node to register AND become ready
	expected := 1 + len(agents)
	progressf("waiting for %d node(s) to become ready...", expected)
	return waitReady(server, expected, 180*time.Second)
}

func withSpace(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return " " + strings.TrimSpace(s)
}

func streamSudo(c *ssh.Client, cmd string, w io.Writer) (int, error) {
	if w != nil {
		return c.Stream(cmd, w)
	}
	_, code, err := c.Run(cmd)
	return code, err
}

// Runner is the minimal command-execution surface vm needs
// (satisfied by *ssh.Client; faked in tests).
type Runner interface {
	Run(cmd string) (string, int, error)
	SudoRun(cmd string) (string, int, error)
}

// serverToken reads /var/lib/rancher/k3s/server/node-token.
func serverToken(server Runner) (string, error) {
	out, code, err := server.SudoRun(`cat /var/lib/rancher/k3s/server/node-token`)
	if err != nil || code != 0 {
		return "", fmt.Errorf("read node-token (exit %d): %s", code, out)
	}
	token := strings.TrimSpace(out)
	if token == "" {
		return "", fmt.Errorf("node-token empty")
	}
	return token, nil
}

// serverInternalIP returns the primary internal IPv4 of the server node.
func serverInternalIP(server Runner) (string, error) {
	out, code, err := server.Run(`hostname -I | awk '{print $1}'`)
	if err != nil || code != 0 {
		return "", fmt.Errorf("hostname -I failed (exit %d): %s", code, out)
	}
	ip := strings.TrimSpace(out)
	if ip == "" {
		return "", fmt.Errorf("no internal IP found")
	}
	return ip, nil
}

// waitReady polls until `expected` nodes have registered and all are Ready.
//
// Counting only the nodes that happen to have registered would let the server
// alone satisfy the wait while agents are still joining, so "cluster ready"
// could be reported for a cluster that is missing most of itself.
func waitReady(server Runner, expected int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lines []string
	for time.Now().Before(deadline) {
		out, code, err := server.SudoRun(`k3s kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'`)
		if err == nil && code == 0 {
			lines = nonEmpty(strings.Split(out, "\n"))
			if len(lines) >= expected && allReady(lines) {
				return nil
			}
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("timeout after %s: %s", timeout, readySummary(lines, expected))
}

// readySummary explains which half of the wait failed, because "registered
// but NotReady" and "never registered" have different causes.
func readySummary(lines []string, expected int) string {
	if len(lines) < expected {
		names := make([]string, 0, len(lines))
		for _, l := range lines {
			name, _, _ := strings.Cut(l, "=")
			names = append(names, name)
		}
		return fmt.Sprintf("only %d of %d node(s) registered (%s); check the agent install output and that they can reach the server on port 6443",
			len(lines), expected, strings.Join(names, ", "))
	}
	var notReady []string
	for _, l := range lines {
		if !strings.HasSuffix(l, "=True") {
			name, _, _ := strings.Cut(l, "=")
			notReady = append(notReady, name)
		}
	}
	return fmt.Sprintf("all %d node(s) registered but not Ready: %s; check `journalctl -u k3s-agent` on those nodes",
		expected, strings.Join(notReady, ", "))
}

func nonEmpty(ss []string) []string {
	var out []string
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func allReady(lines []string) bool {
	if len(lines) == 0 {
		return false
	}
	for _, l := range lines {
		if !strings.HasSuffix(l, "=True") {
			return false
		}
	}
	return true
}

// FetchKubeconfig copies /etc/rancher/k3s/k3s.yaml from the server to
// localPath, rewriting the server address so the file works from here.
//
// k3s writes 127.0.0.1 into its kubeconfig because it expects to be used on
// the node. Copied verbatim to another machine it points kubectl at the
// caller's own loopback, so it must be rewritten to an address that actually
// reaches the server.
func FetchKubeconfig(server *ssh.Client, localPath, serverHost string) error {
	out, code, err := server.SudoRun(`cat /etc/rancher/k3s/k3s.yaml`)
	if err != nil || code != 0 {
		return fmt.Errorf("read k3s.yaml (exit %d): %s", code, out)
	}
	rewritten, err := rewriteKubeconfigServer(out, serverHost)
	if err != nil {
		return err
	}
	return writeFile(localPath, []byte(rewritten))
}

// loopbackServerRe matches the `server:` URL k3s writes for the local node.
var loopbackServerRe = regexp.MustCompile(`(?m)^(\s*server:\s*https://)(127\.0\.0\.1|localhost|\[::1\])(:\d+)`)

// rewriteKubeconfigServer points the kubeconfig at host. A node that is
// genuinely local needs no rewrite, and a kubeconfig already naming a
// routable address is left alone.
func rewriteKubeconfigServer(kubeconfig, host string) (string, error) {
	if host == "" || host == "127.0.0.1" || host == "localhost" || host == "::1" {
		return kubeconfig, nil
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]" // bare IPv6
	}
	out := loopbackServerRe.ReplaceAllString(kubeconfig, "${1}"+host+"${3}")
	if out == kubeconfig && !strings.Contains(kubeconfig, "server: https://"+host) {
		// Nothing matched and the address is not already correct: report it
		// rather than handing back a kubeconfig that will not connect.
		return "", fmt.Errorf("could not find a server address to rewrite in the kubeconfig; "+
			"check it manually and set the cluster server to https://%s:6443", host)
	}
	return out, nil
}
