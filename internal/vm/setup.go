// Package vm installs k3s on target nodes over SSH (k3sup-style bootstrap).
package vm

import (
	"fmt"
	"github.com/solutionforest/k3helper/internal/bundle"
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
	// Channel: stable, latest, or testing. Resolved by update.k3s.io at
	// install time, so it needs that service to be up.
	Channel string
	// Version pins an exact k3s release, e.g. v1.31.2+k3s1. When set it wins
	// over Channel and the install skips update.k3s.io entirely, fetching the
	// build straight from its GitHub release.
	Version string
	// BundleDir installs from a k3s bundle instead of downloading anything.
	// The nodes need no internet at all: see internal/bundle.
	BundleDir string
	// JoinAddress overrides the address the other nodes dial to reach the
	// first server. Needed when k3helper connects over one network and the
	// cluster talks over another — a public IP for SSH, a private one between
	// nodes.
	JoinAddress string
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

// installFailure explains why an install step did not work.
//
// A step fails in two different ways and they need different words: the
// connection itself broke (err is non-nil), or the command ran and exited
// non-zero (err is nil, and the reason is in the output already streamed to
// the operator). Passing a nil error to %w printed "%!w(<nil>)" where the
// reason should have been — seen against a real VM, and it is the last thing
// on screen when an install fails.
func installFailure(what string, code int, err error) error {
	if err != nil {
		return fmt.Errorf("%s failed: %w", what, err)
	}
	return fmt.Errorf("%s failed (exit %d) — the install output above says why", what, code)
}

func (o Options) channel() string {
	if o.Channel == "" {
		return "stable"
	}
	return o.Channel
}

// release renders the environment that tells the k3s installer which build to
// fetch: either an exact version, or a channel to resolve.
//
// A channel is a lookup against update.k3s.io, which is a second service that
// has to be up and correctly certificated. It was neither during live testing
// — it served a Traefik default certificate from all three of its addresses,
// so every `curl -sfL https://get.k3s.io | sh -` on the internet failed TLS
// verification and then failed to download. Pinning a version skips that
// service entirely and fetches straight from the GitHub release, which was
// healthy throughout.
func (o Options) release() string {
	if o.Version != "" {
		return "INSTALL_K3S_VERSION=" + shellQuote(o.Version)
	}
	return "INSTALL_K3S_CHANNEL=" + o.channel()
}

// offline reports whether this install comes from a bundle.
func (o Options) offline() bool { return o.BundleDir != "" }

// serverInstallCmd builds the first server's install command.
//
// The online and offline forms differ only in where the installer and the
// binary come from; the arguments after `server` are identical, so they are
// built once here rather than in three places that would drift.
func (o Options) serverInstallCmd(t Target, tokenArg, initArgs string) string {
	if o.offline() {
		return offlineInstallCmd(t.Client.SudoPrefix(), strings.TrimSpace(tokenArg)+" ", "server", withSpace(initArgs))
	}
	return fmt.Sprintf(
		`curl -sfL %s | %s%s%s sh -s - server%s`,
		o.installURL(), t.Client.SudoPrefix(), o.release(), tokenArg, withSpace(initArgs),
	)
}

func (o Options) joinServerCmd(t Target, token, ip string) string {
	args := fmt.Sprintf(" --server https://%s:6443%s", ip, withSpace(o.ServerExtraArgs))
	if o.offline() {
		return offlineInstallCmd(t.Client.SudoPrefix(),
			"K3S_TOKEN="+shellQuote(token)+" ", "server", args)
	}
	return fmt.Sprintf(
		`curl -sfL %s | %sK3S_TOKEN=%s %s sh -s - server%s`,
		o.installURL(), t.Client.SudoPrefix(), shellQuote(token), o.release(), args,
	)
}

func (o Options) agentInstallCmd(t Target, token, ip string) string {
	env := fmt.Sprintf("K3S_URL=https://%s:6443 K3S_TOKEN=%s ", ip, shellQuote(token))
	if o.offline() {
		return offlineInstallCmd(t.Client.SudoPrefix(), env, "agent", withSpace(o.AgentExtraArgs))
	}
	return fmt.Sprintf(
		`curl -sfL %s | %s%s%s sh -s - agent%s`,
		o.installURL(), t.Client.SudoPrefix(), env, o.release(), withSpace(o.AgentExtraArgs),
	)
}

// Host is what Setup needs from a connection. *ssh.Client satisfies it; tests
// substitute a recorder so the install commands can be asserted without a VM.
type Host interface {
	Run(cmd string) (string, int, error)
	SudoRun(cmd string) (string, int, error)
	Stream(cmd string, w io.Writer) (int, error)
	SudoPrefix() string
}

// Target is one machine to bootstrap, with an open connection to it.
type Target struct {
	Node   ssh.Node
	Client Host
}

// Compile-time check that the real client still fits.
var _ Host = (*ssh.Client)(nil)

// Setup installs k3s across the cluster: servers first, then agents.
//
// One server keeps k3s's default sqlite datastore. Two or more switch to
// embedded etcd: the first is installed with --cluster-init and the rest join
// it with --server. etcd needs an odd number of members to hold quorum, so an
// even count is reported rather than silently built.
func Setup(servers []Target, agents []Target, opts Options) error {
	progressf := func(format string, a ...interface{}) {
		if opts.Progress != nil {
			fmt.Fprintf(opts.Progress, format+"\n", a...)
		}
	}
	// An offline install has to put the bundle on every node before anything
	// is installed anywhere. Doing it per node as we go would leave a cluster
	// half built when the last node turns out to be missing a file that was
	// never in the bundle to begin with.
	if opts.offline() {
		m, err := bundle.Load(opts.BundleDir)
		if err != nil {
			return err
		}
		progressf("offline install from %s (k3s %s, %s)", opts.BundleDir, m.Version, m.Arch)
		for _, t := range append(append([]Target{}, servers...), agents...) {
			if err := stageBundle(t, opts.BundleDir, m, opts.Progress); err != nil {
				return err
			}
		}
	}
	if len(servers) == 0 {
		return fmt.Errorf("at least one server is required")
	}
	if len(servers)%2 == 0 {
		return fmt.Errorf(
			"%d servers cannot hold etcd quorum: a cluster of %d tolerates the same "+
				"single failure as %d, and loses quorum at two. Use an odd number (1, 3 or 5)",
			len(servers), len(servers), len(servers)-1)
	}

	first := servers[0]
	ha := len(servers) > 1

	// 1. first server. --cluster-init switches k3s from sqlite to embedded
	// etcd; it must be given only to the node that creates the cluster.
	mode := "single-server (sqlite)"
	initArgs := opts.ServerExtraArgs
	if ha {
		mode = fmt.Sprintf("HA (%d servers, embedded etcd)", len(servers))
		initArgs = strings.TrimSpace("--cluster-init " + opts.ServerExtraArgs)
	}
	progressf("[%s] installing k3s server — %s...", first.Node.Host, mode)
	tokenArg := ""
	if opts.Token != "" {
		tokenArg = " K3S_TOKEN=" + shellQuote(opts.Token)
	}
	cmd := opts.serverInstallCmd(first, tokenArg, initArgs)
	if code, err := streamSudo(first.Client, cmd, opts.Progress); err != nil || code != 0 {
		return installFailure("server install", code, err)
	}

	// 2. the join token and the address the others will reach it on
	progressf("[%s] fetching join token...", first.Node.Host)
	token, err := serverToken(first.Client)
	if err != nil {
		return fmt.Errorf("fetch token: %w", err)
	}
	ip, err := opts.joinAddress(first, first.Client)
	if err != nil {
		return fmt.Errorf("determine the address the other nodes join on: %w", err)
	}
	progressf("[%s] other nodes will join at https://%s:6443", first.Node.Host, ip)

	// 3. remaining servers join the etcd cluster
	for _, s := range servers[1:] {
		progressf("[%s] joining as server (etcd member)...", s.Node.Host)
		joinCmd := opts.joinServerCmd(s, token, ip)
		if code, err := streamSudo(s.Client, joinCmd, opts.Progress); err != nil || code != 0 {
			return installFailure("server "+s.Node.Host+" join", code, err)
		}
		// Servers are added one at a time on purpose: etcd learners join
		// sequentially, and adding several at once can cost quorum.
		if err := waitReady(first.Client, 0, 120*time.Second); err != nil {
			return fmt.Errorf("cluster did not settle after %s joined: %w", s.Node.Host, err)
		}
	}

	// 4. agents
	for _, a := range agents {
		progressf("[%s] installing k3s agent...", a.Node.Host)
		joinCmd := opts.agentInstallCmd(a, token, ip)
		if code, err := streamSudo(a.Client, joinCmd, opts.Progress); err != nil || code != 0 {
			return installFailure("agent "+a.Node.Host+" install", code, err)
		}
	}

	// 5. wait for every node to register AND become ready
	expected := len(servers) + len(agents)
	progressf("waiting for %d node(s) to become ready...", expected)
	return waitReady(first.Client, expected, 180*time.Second)
}

// shellQuote wraps a value in single quotes, escaping any it contains. These
// strings are piped into a root shell, so an unquoted token containing a
// space, ; or $( ) would break the command or run as one.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func withSpace(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return " " + strings.TrimSpace(s)
}

func streamSudo(c Host, cmd string, w io.Writer) (int, error) {
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

// joinAddress is the address agents and joining servers dial to reach the
// first server.
//
// It is the address from the targets file, not one discovered on the node.
// The operator chose it and k3helper has just proved it works by connecting
// over it, whereas `hostname -I` returns whatever the machine lists first —
// which on a cloud VM is the public address. That address is often the one
// thing the cluster's own network cannot use: an air-gapped node with egress
// blocked can reach its neighbour's private IP and nothing else, so the agents
// sat retrying "failed to get CA certs" against a public IP forever while the
// server ran perfectly well beside them.
//
// Discovery remains the fallback for a local node, which has no host address
// to take.
func (o Options) joinAddress(first Target, client Runner) (string, error) {
	if o.JoinAddress != "" {
		return o.JoinAddress, nil
	}
	h := strings.TrimSpace(first.Node.Host)
	if h != "" && !first.Node.Local && h != "localhost" && h != "127.0.0.1" {
		return h, nil
	}
	return serverInternalIP(client)
}

// waitReady polls until `expected` nodes have registered and all are Ready.
// An expected of 0 means "however many are registered, all must be Ready",
// which is what the pause between etcd members joining needs.
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

// FetchKubeadmKubeconfig copies /etc/kubernetes/admin.conf from the control
// plane, rewriting the server address the same way as the k3s path.
func FetchKubeadmKubeconfig(server *ssh.Client, localPath, serverHost string) error {
	out, code, err := server.SudoRun(`cat /etc/kubernetes/admin.conf`)
	if err != nil || code != 0 {
		return fmt.Errorf("read admin.conf (exit %d): %s", code, out)
	}
	rewritten, err := rewriteKubeconfigServer(out, serverHost)
	if err != nil {
		return err
	}
	return writeFile(localPath, []byte(rewritten))
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
