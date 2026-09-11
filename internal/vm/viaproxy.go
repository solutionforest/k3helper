package vm

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/solutionforest/k3helper/internal/proxy"
)

// This file lends a node the operator's internet connection for the length of
// an install.
//
// k3s can be installed from a bundle because it is one static binary and one
// image archive. kubeadm cannot: it needs apt packages from a distribution
// mirror and images from registry.k8s.io, and neither fits in a file you carry
// in. The machine running k3helper usually can reach both, and it already
// holds an SSH connection to every node — so the connection carries the
// traffic backwards, for as long as the install takes and no longer.
//
// This is deliberately not the default. An air-gapped network is air-gapped on
// purpose, and in some environments opening even a temporary, proxied,
// allowlisted route would be a policy breach regardless of how briefly it
// existed. It happens only when asked for by name.

// Tunneler is a Host that can open a listener on the node. Kept apart from
// Host because only this path needs it.
type Tunneler interface {
	ListenRemote(addr string) (net.Listener, error)
}

// nodeProxy is a live tunnel to one node.
type nodeProxy struct {
	host     string
	port     int
	listener net.Listener
	server   *proxy.Server
	client   Host
}

// aptProxyConf is where the apt configuration is written. The name sorts late
// so it wins over anything already there, and says who wrote it.
const aptProxyConf = "/etc/apt/apt.conf.d/99-k3helper-proxy"

// containerdProxyConf is a systemd drop-in, which is how containerd is given
// an environment at all: it does not read the invoking shell's.
const containerdProxyConf = "/etc/systemd/system/containerd.service.d/k3helper-proxy.conf"

// startNodeProxy opens the tunnel and points the node's package manager and
// container runtime at it.
func startNodeProxy(t Target, allow []string, progress io.Writer) (*nodeProxy, error) {
	tun, ok := t.Client.(Tunneler)
	if !ok {
		return nil, fmt.Errorf("node %s: this connection cannot open a tunnel", t.Node.Host)
	}
	// Port 0: the node picks a free one. A fixed port collides with whatever
	// is already listening on a machine we do not own.
	l, err := tun.ListenRemote("127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("node %s: %w", t.Node.Host, err)
	}
	port := 0
	if a, ok := l.Addr().(*net.TCPAddr); ok {
		port = a.Port
	}
	if port == 0 {
		l.Close()
		return nil, fmt.Errorf("node %s: the tunnel did not report a port", t.Node.Host)
	}

	// One line per refused host, not per refused request: apt retries, and a
	// single unreachable repository otherwise fills the install log with the
	// same sentence eight times.
	var refusedMu sync.Mutex
	refused := map[string]bool{}
	srv := &proxy.Server{
		Allow: allow,
		OnRefuse: func(host string) {
			refusedMu.Lock()
			first := !refused[host]
			refused[host] = true
			refusedMu.Unlock()
			if first && progress != nil {
				fmt.Fprintf(progress,
					"[%s] proxy refused %s — not on the allowlist; add it with --proxy-allow %s if the install needs it\n",
					t.Node.Host, host, host)
			}
		},
	}
	go srv.Serve(l)

	p := &nodeProxy{host: t.Node.Host, port: port, listener: l, server: srv, client: t.Client}
	if err := p.configure(); err != nil {
		p.Stop()
		return nil, err
	}
	if progress != nil {
		fmt.Fprintf(progress, "[%s] lending this machine's connection on 127.0.0.1:%d for the install\n",
			t.Node.Host, port)
	}
	return p, nil
}

// configure writes the proxy settings the install actually reads.
func (p *nodeProxy) configure() error {
	url := fmt.Sprintf("http://127.0.0.1:%d", p.port)

	// apt needs both: https goes through the same proxy with CONNECT.
	// Pipelining off, and a timeout on.
	//
	// apt sends several requests down one connection without waiting for the
	// replies, and that is the first thing to disable against any proxy it
	// finds flaky — over an SSH channel, index fetches were timing out.
	//
	// The timeout matters more. This proxy lives inside a running k3helper: if
	// the operator's machine goes away mid-install — a closed laptop, a
	// dropped link, a Ctrl-C — apt has no timeout of its own and waits on the
	// dead tunnel forever, holding the apt lock while it does. One was found
	// still holding it after 31 minutes, which then failed every later run on
	// that node with a lock error that pointed nowhere near the cause.
	apt := fmt.Sprintf(`Acquire::http::Proxy "%s";
Acquire::https::Proxy "%s";
Acquire::http::Pipeline-Depth "0";
Acquire::Retries "3";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
`, url, url)
	if out, code, err := p.client.SudoRun(fmt.Sprintf(
		`mkdir -p /etc/apt/apt.conf.d && printf %%s %s > %s`,
		shellQuote(apt), aptProxyConf)); err != nil || code != 0 {
		return fmt.Errorf("node %s: write apt proxy config (exit %d): %s", p.host, code, out)
	}

	// containerd is a service: it reads its environment from systemd, not from
	// whatever shell started the install.
	drop := fmt.Sprintf(`[Service]
Environment="HTTP_PROXY=%s"
Environment="HTTPS_PROXY=%s"
Environment="NO_PROXY=%s"
`, url, url, noProxy)
	if out, code, err := p.client.SudoRun(fmt.Sprintf(
		`mkdir -p %s && printf %%s %s > %s && systemctl daemon-reload && systemctl restart containerd 2>/dev/null || true`,
		dirOf(containerdProxyConf), shellQuote(drop), containerdProxyConf)); err != nil || code != 0 {
		return fmt.Errorf("node %s: write containerd proxy config (exit %d): %s", p.host, code, out)
	}
	return nil
}

// noProxy is everything that must not go through the tunnel.
//
// Cluster traffic is the point of this list. kubeadm talks to the API server
// it has just started, and with only localhost exempted it sent that through
// the proxy — which refused it, because the node's own address is not on an
// allowlist of internet hosts, and the install failed. Routing a node's
// conversation with itself through the operator's laptop would be absurd even
// if it worked.
//
// The private ranges cover the node's own addresses without having to know
// them. Go's proxy resolution understands CIDR here, and every tool in this
// install is Go.
const noProxy = "localhost,127.0.0.1,::1," +
	"10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16," +
	".svc,.svc.cluster.local,.cluster.local"

// Env is the proxy environment for commands run during the install, for the
// ones that read it directly — curl fetching a repository key, kubeadm
// fetching a version marker.
func (p *nodeProxy) Env() string {
	url := fmt.Sprintf("http://127.0.0.1:%d", p.port)
	// Both cases: Go reads the upper-case names, curl and apt the lower-case.
	return fmt.Sprintf(
		`http_proxy=%s https_proxy=%s HTTP_PROXY=%s HTTPS_PROXY=%s `+
			`no_proxy=%s NO_PROXY=%s `,
		url, url, url, url, shellQuote(noProxy), shellQuote(noProxy))
}

// Stop closes the tunnel and removes every trace of it from the node.
//
// The removal matters more than the close: a node left pointing at a proxy
// that no longer exists cannot reach its own package manager afterwards, and
// the symptom — apt hanging on 127.0.0.1 — points nowhere near k3helper.
func (p *nodeProxy) Stop() {
	if p.client != nil {
		p.client.SudoRun(fmt.Sprintf(
			`rm -f %s %s; systemctl daemon-reload 2>/dev/null; systemctl restart containerd 2>/dev/null || true`,
			aptProxyConf, containerdProxyConf))
	}
	if p.server != nil {
		p.server.Close()
	}
	if p.listener != nil {
		p.listener.Close()
	}
}

// startProxies opens a tunnel to every node, unwinding the ones already open
// if a later node fails.
func startProxies(targets []Target, allow []string, progress io.Writer) ([]*nodeProxy, error) {
	var out []*nodeProxy
	for _, t := range targets {
		p, err := startNodeProxy(t, allow, progress)
		if err != nil {
			stopProxies(out)
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func stopProxies(ps []*nodeProxy) {
	for _, p := range ps {
		p.Stop()
	}
}

func dirOf(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return "/"
}

// proxyEnv is the environment prefix for a command on one node, empty when no
// tunnel is open for it.
//
// The prefix is needed on top of the apt and containerd configuration because
// some of the install is neither: the kubeadm prerequisites fetch a repository
// key with curl, which reads http_proxy from its own environment and nothing
// else.
func proxyEnv(ps []*nodeProxy, host string) string {
	for _, p := range ps {
		if p.host == host {
			return p.Env()
		}
	}
	return ""
}
