package proxy

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// nodeConn behaves like the connections x/crypto/ssh hands back from a
// reverse tunnel: a net.Conn that refuses deadlines.
type nodeConn struct{ net.Conn }

func (nodeConn) SetDeadline(time.Time) error {
	return errors.New("ssh: tcpChan: deadline not supported")
}
func (nodeConn) SetReadDeadline(time.Time) error {
	return errors.New("ssh: tcpChan: deadline not supported")
}
func (nodeConn) SetWriteDeadline(time.Time) error {
	return errors.New("ssh: tcpChan: deadline not supported")
}

type nodeListener struct{ net.Listener }

func (l nodeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return nodeConn{c}, nil
}

// The connections this proxy serves arrive over an SSH reverse tunnel, and
// x/crypto/ssh channels refuse to set deadlines. Served through an
// http.Server, CONNECT hangs on that: Hijack calls abortPendingRead, which
// interrupts its background read by setting a deadline in the past, and on a
// connection that cannot do that it waits forever and never replies.
//
// Found against real air-gapped nodes, where every https fetch through the
// tunnel failed with "Proxy CONNECT aborted due to timeout" while plain http
// went through — because only https uses CONNECT.
func TestConnectOverADeadlinelessConn(t *testing.T) {
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		c, err := echo.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()

	raw, _ := net.Listen("tcp", "127.0.0.1:0")
	s := &Server{Allow: []string{"127.0.0.1"}}
	go s.Serve(nodeListener{raw})
	defer func() { s.Close(); raw.Close() }()

	c, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, "CONNECT "+echo.Addr().String()+" HTTP/1.1\r\nHost: "+echo.Addr().String()+"\r\n\r\nping")
	br := bufio.NewReader(c)
	r, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("no response to CONNECT over a deadlineless conn: %v", err)
	}
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatalf("tunnel stalled: %v", err)
	}
	t.Logf("got %q", buf)
}
