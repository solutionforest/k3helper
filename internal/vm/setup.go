// Package vm installs k3s on target nodes over SSH (k3sup-style bootstrap).
package vm

import (
	"fmt"
	"io"
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

	// 5. wait for nodes to become ready
	progressf("waiting for nodes to become ready...")
	return waitReady(server, 120*time.Second)
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

// waitReady polls `kubectl get nodes` on the server until all are Ready or timeout.
func waitReady(server Runner, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, code, err := server.SudoRun(`k3s kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'`)
		if err == nil && code == 0 {
			lines := nonEmpty(strings.Split(out, "\n"))
			if len(lines) > 0 && allReady(lines) {
				return nil
			}
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("timeout waiting for nodes Ready after %s", timeout)
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

// FetchKubeconfig copies /etc/rancher/k3s/k3s.yaml from the server to local path.
func FetchKubeconfig(server *ssh.Client, localPath string) error {
	out, code, err := server.SudoRun(`cat /etc/rancher/k3s/k3s.yaml`)
	if err != nil || code != 0 {
		return fmt.Errorf("read k3s.yaml (exit %d): %s", code, out)
	}
	return writeFile(localPath, []byte(out))
}
