// Package proxy is a small HTTP forward proxy, used to lend a node the
// operator's own internet connection for the length of an install.
//
// An air-gapped node cannot reach a distribution mirror or a container
// registry, and for kubeadm that is fatal: unlike k3s it needs apt packages
// and images from registry.k8s.io, neither of which fits in a bundle. The
// machine running k3helper usually *can* reach those, and it already holds an
// SSH connection to every node — so the connection can carry the traffic
// backwards.
//
// This deliberately does less than a general proxy:
//
//   - Only CONNECT and absolute-form requests, which is all apt and containerd
//     produce.
//   - A host allowlist, checked before anything is dialled. Lending a machine
//     a route to the internet is not the same as lending it a route to
//     everything, and an air-gapped network is air-gapped for a reason.
//   - Nothing is cached, logged to disk, or modified in flight.
package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultAllow is what a Kubernetes install actually needs to reach.
//
// Matching is on the host, by exact name or by dot-suffix, so "pkg.dev" admits
// "us-central1-docker.pkg.dev" and nothing that merely ends in those letters.
var DefaultAllow = []string{
	// distribution packages, including the mirrors cloud images actually ship
	// pointing at — a stock Ubuntu image on DigitalOcean uses the provider's
	// own, which the first live run of this proxy refused.
	"archive.ubuntu.com", "security.ubuntu.com", "ports.ubuntu.com",
	"deb.debian.org", "security.debian.org",
	"mirrors.digitalocean.com", "repos-droplet.digitalocean.com",
	// Ubuntu Pro's ESM endpoint is configured on stock images whether or not
	// the machine is subscribed; refusing it makes apt retry and fail noisily
	// for something the install does not even need.
	"esm.ubuntu.com", "motd.ubuntu.com", "changelogs.ubuntu.com",
	"mirrors.linode.com", "mirror.hetzner.com", "azure.archive.ubuntu.com",
	"clouds.archive.ubuntu.com", "ec2.archive.ubuntu.com",
	"europe-west1.gce.archive.ubuntu.com", "gce.archive.ubuntu.com",
	// kubernetes packages and images
	// pkgs.k8s.io redirects the actual packages to a CDN on another name, and
	// registry.k8s.io redirects images to a cloud provider's registry. An
	// allowlist with only the names an operator would think to write refuses
	// the download that follows the redirect.
	// dl.k8s.io serves the version markers kubeadm reads during init.
	"pkgs.k8s.io", "packages.k8s.io", "registry.k8s.io", "dl.k8s.io",
	"pkg.dev", "storage.googleapis.com", "amazonaws.com", "cloudfront.net",
	// k3s
	"get.k3s.io", "update.k3s.io", "github.com", "githubusercontent.com",
	// docker hub, for CNI and workload images
	"docker.io", "docker.com", "cloudflare.docker.com",
	// quay and ghcr, where the CNIs live. Flannel's images are on ghcr.io and
	// its blobs on pkg-containers.githubusercontent.com — an allowlist built
	// from "what Kubernetes needs" misses both, and the cluster then installs
	// perfectly and sits NotReady because its network plugin cannot start.
	"quay.io", "ghcr.io", "pkg-containers.githubusercontent.com",
}

// Server forwards requests from a node to the internet.
type Server struct {
	// Allow lists permitted hosts. Empty means DefaultAllow; the single entry
	// "*" means anything, which the caller has to ask for explicitly.
	Allow []string
	// Dial is overridden by tests.
	Dial func(network, addr string) (net.Conn, error)
	// OnRefuse is called with the host of a rejected request, so the caller
	// can tell an operator why an install could not fetch something.
	OnRefuse func(host string)

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

func (s *Server) allowed(host string) bool {
	h := bareHost(host)
	list := s.Allow
	if len(list) == 0 {
		list = DefaultAllow
	}
	for _, a := range list {
		if a == "*" {
			return true
		}
		// The entry is stripped too. A CONNECT names host:port, so an operator
		// copying one out of a refusal would otherwise add "host:443" to the
		// allowlist and find it still refused.
		a = bareHost(a)
		if h == a || strings.HasSuffix(h, "."+a) {
			return true
		}
	}
	return false
}

// bareHost lowercases a host and drops any port.
func bareHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return h
}

func (s *Server) dial(addr string) (net.Conn, error) {
	if s.Dial != nil {
		return s.Dial("tcp", addr)
	}
	return net.DialTimeout("tcp", addr, 30*time.Second)
}

// Serve reads the proxy protocol off each connection itself rather than
// handing the listener to an http.Server.
//
// That is not a preference, it is a requirement. These connections arrive over
// an SSH reverse tunnel, and x/crypto/ssh's channels do not support deadlines.
// http.Server's CONNECT path goes through Hijack, which calls
// abortPendingRead, which interrupts its background read by setting a deadline
// in the past — on a connection that cannot do that, it waits for a read that
// will never be interrupted and the handler never replies. The client sees
// "Proxy CONNECT aborted due to timeout" and the install stops, which is
// exactly what happened the first time this ran against real nodes.
//
// Reading the request directly is also simply less machinery: a forward proxy
// needs a request line, a host, and two io.Copys.
func (s *Server) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		if !s.track(c) {
			c.Close()
			return net.ErrClosed
		}
		go func() {
			defer s.done(c)
			s.handle(c)
		}()
	}
}

func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if s.conns == nil {
		s.conns = map[net.Conn]struct{}{}
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) done(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	c.Close()
}

// Close stops the proxy and drops every connection it is carrying. Safe to
// call before Serve, after it, or twice.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	return nil
}

// handle serves one connection, which may carry several requests: apt reuses
// a connection for every index and package it fetches.
func (s *Server) handle(c net.Conn) {
	br := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if req.Method == http.MethodConnect {
			// CONNECT takes the connection over for good; nothing follows it.
			s.connect(c, br, req)
			return
		}
		if !s.forward(c, req) {
			return
		}
	}
}

// connect splices the connection to the destination.
//
// The reader carries anything already buffered — a client that pipelines, as
// curl does when it sends its TLS ClientHello straight after CONNECT without
// waiting for the 200, has those bytes read before the reply is even written.
// Splicing the bare connection instead of the reader drops them, and the
// handshake then waits for a hello that was received and discarded.
func (s *Server) connect(c net.Conn, br *bufio.Reader, req *http.Request) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	if !s.allowed(host) {
		s.refuseConn(c, host)
		return
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "443")
	}
	upstream, err := s.dial(host)
	if err != nil {
		fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer upstream.Close()

	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(upstream, br); closeWrite(upstream) }()
	go func() { defer wg.Done(); io.Copy(c, upstream); closeWrite(c) }()
	wg.Wait()
}

// forward handles the plain-http absolute-form request apt uses for a mirror
// that is not behind TLS. It reports whether the connection can carry another.
func (s *Server) forward(c net.Conn, req *http.Request) bool {
	if !req.URL.IsAbs() {
		writeError(c, http.StatusBadRequest,
			"proxy: this is a forward proxy; requests must name a full URL")
		return false
	}
	if !s.allowed(req.URL.Host) {
		s.refuseConn(c, req.URL.Host)
		return false
	}
	out := req.Clone(req.Context())
	out.RequestURI = ""
	// Hop-by-hop headers do not belong on the outbound request.
	for _, h := range []string{"Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization"} {
		out.Header.Del(h)
	}
	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		writeError(c, http.StatusBadGateway, "proxy: "+err.Error())
		return false
	}
	defer resp.Body.Close()
	if err := resp.Write(c); err != nil {
		return false
	}
	return !resp.Close && !req.Close
}

func writeError(c net.Conn, status int, body string) {
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}

func (s *Server) refuseConn(c net.Conn, host string) {
	// Reported without the port, because that is what goes in the allowlist.
	host = bareHost(host)
	if s.OnRefuse != nil {
		s.OnRefuse(host)
	}
	writeError(c, http.StatusForbidden, fmt.Sprintf(
		"proxy: %s is not on the allowlist. k3helper lends a node its own connection for "+
			"the length of an install, not general internet access; pass --proxy-allow %s to permit it.",
		host, host))
}

// closeWrite half-closes where the connection supports it, so the far end sees
// a clean end of stream rather than a reset.
func closeWrite(c net.Conn) {
	type cw interface{ CloseWrite() error }
	if v, ok := c.(cw); ok {
		v.CloseWrite()
		return
	}
	c.Close()
}
