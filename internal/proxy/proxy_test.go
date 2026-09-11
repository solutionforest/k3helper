package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serve starts a proxy on a local listener and returns its address.
func serve(t *testing.T, s *Server) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l)
	t.Cleanup(func() { s.Close(); l.Close() })
	return l.Addr().String()
}

func TestAllowlistMatching(t *testing.T) {
	s := &Server{Allow: []string{"pkgs.k8s.io", "pkg.dev"}}
	tests := []struct {
		host string
		want bool
	}{
		{"pkgs.k8s.io", true},
		{"pkgs.k8s.io:443", true},
		{"us-central1-docker.pkg.dev", true},
		{"PKGS.K8S.IO", true},
		{"evil.com", false},
		// A suffix match must be on a dot boundary, or "notpkg.dev" would pass
		// for "pkg.dev" and an allowlist would mean very little.
		{"notpkg.dev", false},
		{"pkgs.k8s.io.evil.com", false},
	}
	for _, tc := range tests {
		if got := s.allowed(tc.host); got != tc.want {
			t.Errorf("allowed(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestAllowStarPermitsAnything(t *testing.T) {
	s := &Server{Allow: []string{"*"}}
	if !s.allowed("anything.example") {
		t.Error(`Allow "*" did not permit an arbitrary host`)
	}
}

func TestDefaultAllowCoversWhatAnInstallNeeds(t *testing.T) {
	s := &Server{}
	for _, host := range []string{
		"archive.ubuntu.com", "security.ubuntu.com",
		"pkgs.k8s.io", "registry.k8s.io",
		"us-central1-docker.pkg.dev", "storage.googleapis.com",
		"get.k3s.io", "github.com", "objects.githubusercontent.com",
		"registry-1.docker.io", "production.cloudflare.docker.com",
	} {
		if !s.allowed(host) {
			t.Errorf("an install needs %s and the default allowlist refuses it", host)
		}
	}
	if s.allowed("example.com") {
		t.Error("the default allowlist is not a list at all")
	}
}

// The refusal has to say what to do about it, or an install fails against a
// mirror nobody thought to name and the operator has nothing to go on.
func TestRefusalExplainsItself(t *testing.T) {
	var refused string
	s := &Server{Allow: []string{"pkgs.k8s.io"}, OnRefuse: func(h string) { refused = h }}
	addr := serve(t, s)

	// Driven directly rather than through a Transport, which would try to
	// resolve nexus.corp itself before the proxy ever saw it.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET http://nexus.corp/ubuntu/dists/noble/Release HTTP/1.1\r\nHost: nexus.corp\r\n\r\n")
	br := bufio.NewReader(c)
	r, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", r.StatusCode)
	}
	body, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(body), "--proxy-allow") {
		t.Errorf("the refusal does not say how to permit it: %s", body)
	}
	if refused != "nexus.corp" {
		t.Errorf("OnRefuse got %q, want nexus.corp", refused)
	}
}

// A plain http fetch is what apt does against a mirror without TLS.
func TestForwardsAbsoluteFormRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Origin: Ubuntu\n"))
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")

	s := &Server{Allow: []string{"127.0.0.1"}}
	addr := serve(t, s)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET "+upstream.URL+"/dists/noble/Release HTTP/1.1\r\nHost: "+host+"\r\n\r\n")
	r, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(body), "Origin: Ubuntu") {
		t.Errorf("body = %q", body)
	}
}

// CONNECT is what apt and containerd use for https, and the tunnel is what
// makes an air-gapped node able to fetch at all — the proxy resolves the name,
// so the node does not need working DNS.
func TestConnectTunnelsBytesBothWays(t *testing.T) {
	// A trivial echo server standing in for the far end of the tunnel.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		c, err := echo.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()

	s := &Server{Allow: []string{"127.0.0.1"}}
	addr := serve(t, s)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "CONNECT "+echo.Addr().String()+" HTTP/1.1\r\nHost: "+echo.Addr().String()+"\r\n\r\n")
	br := bufio.NewReader(c)
	r, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT returned %d", r.StatusCode)
	}
	io.WriteString(c, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Errorf("tunnel returned %q, want ping", buf)
	}
}

// A CONNECT to a host that is not allowed must be refused before anything is
// dialled — the point of the allowlist is that the connection is never made.
func TestConnectRefusedBeforeDialling(t *testing.T) {
	dialled := false
	s := &Server{
		Allow: []string{"pkgs.k8s.io"},
		Dial: func(n, a string) (net.Conn, error) {
			dialled = true
			return nil, nil
		},
	}
	addr := serve(t, s)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "CONNECT evil.example:443 HTTP/1.1\r\nHost: evil.example:443\r\n\r\n")
	r, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", r.StatusCode)
	}
	if dialled {
		t.Error("a refused host was dialled anyway")
	}
}

// curl does not wait for the 200 before sending its TLS ClientHello, so those
// bytes are already in the http server's buffer when the connection is
// hijacked. Reading from the bare connection instead of that buffer drops
// them, and the far end then waits forever for a hello it was sent.
//
// Found live: every https fetch through the tunnel failed with "Proxy CONNECT
// aborted due to timeout" while plain http went through fine, because only the
// https path uses CONNECT.
func TestConnectKeepsBytesSentBeforeTheResponse(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		c, err := echo.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()

	s := &Server{Allow: []string{"127.0.0.1"}}
	addr := serve(t, s)

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// The request and the payload go out together, in one write, exactly as a
	// pipelining client sends them.
	io.WriteString(c, "CONNECT "+echo.Addr().String()+" HTTP/1.1\r\nHost: "+
		echo.Addr().String()+"\r\n\r\nhello-before-200")

	br := bufio.NewReader(c)
	r, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT returned %d", r.StatusCode)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, len("hello-before-200"))
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("the bytes sent before the 200 were dropped: %v", err)
	}
	if string(buf) != "hello-before-200" {
		t.Errorf("tunnel returned %q", buf)
	}
}

// A stock cloud image points apt at its provider's mirror, not at
// archive.ubuntu.com. The first live run refused DigitalOcean's and stopped
// the install.
func TestDefaultAllowCoversCloudProviderMirrors(t *testing.T) {
	s := &Server{}
	for _, host := range []string{
		"repos-droplet.digitalocean.com",
		"mirrors.digitalocean.com",
		"us-east-1.ec2.archive.ubuntu.com",
		"azure.archive.ubuntu.com",
	} {
		if !s.allowed(host) {
			t.Errorf("a stock cloud image installs from %s and the allowlist refuses it", host)
		}
	}
}

// A refusal names a host:port, and an operator copies that straight into
// --proxy-allow. If the entry is not stripped the same way the request is, the
// host they just permitted is refused again.
func TestAllowlistEntriesMayCarryAPort(t *testing.T) {
	s := &Server{Allow: []string{"prod-cdn.packages.k8s.io:443"}}
	if !s.allowed("prod-cdn.packages.k8s.io:443") {
		t.Error("an entry with a port does not match the request it was copied from")
	}
	if !s.allowed("prod-cdn.packages.k8s.io") {
		t.Error("an entry with a port does not match the bare host")
	}
}

// The package repositories redirect the actual download elsewhere, and an
// allowlist that only knows the name an operator would write refuses whatever
// the redirect points at. Both were hit live.
func TestDefaultAllowFollowsRedirectTargets(t *testing.T) {
	s := &Server{}
	for _, host := range []string{
		"prod-cdn.packages.k8s.io",
		"us-central1-docker.pkg.dev",
		"d1.cloudfront.net",
	} {
		if !s.allowed(host) {
			t.Errorf("%s is where a redirect lands and the allowlist refuses it", host)
		}
	}
}

// The CNI's own images are the ones an allowlist built from "what Kubernetes
// needs" forgets. Live, every control-plane image pulled and the cluster still
// sat NotReady, because flannel is on ghcr.io.
func TestDefaultAllowCoversCNIImages(t *testing.T) {
	s := &Server{}
	for _, host := range []string{
		"ghcr.io", "pkg-containers.githubusercontent.com", "quay.io",
	} {
		if !s.allowed(host) {
			t.Errorf("a cluster cannot start its network plugin without %s", host)
		}
	}
}
