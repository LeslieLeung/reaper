package repository

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
	gitHTTP "github.com/go-git/go-git/v5/plumbing/transport/http"
)

const gitNetworkIdleTimeout = 5 * time.Minute

var errGitIdleTimeout = errors.New("git network idle timeout")

func init() {
	// go-git selects transports from a process-wide registry. Install once at
	// startup, before any concurrent syncs, and leave other HTTP clients alone.
	gitClient := gitHTTP.NewClient(newGitHTTPClient(gitNetworkIdleTimeout))
	client.InstallProtocol("http", gitClient)
	client.InstallProtocol("https", gitClient)
}

func newGitHTTPClient(idleTimeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// A read deadline belongs to a connection. Use HTTP/1.1 so concurrent
	// transfers never share one socket: traffic from another repository must
	// not conceal a stalled clone via HTTP/2 multiplexing.
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &gitIdleConn{Conn: conn, idleTimeout: idleTimeout}, nil
	}
	// No http.Client.Timeout: receiving bytes keeps a transfer alive regardless
	// of its total duration. Cloning the default transport preserves proxy,
	// TLS, pooling, and connection-establishment settings (except HTTP/2).
	return &http.Client{Transport: tr}
}

type gitIdleConn struct {
	net.Conn
	idleTimeout time.Duration
}

func (c *gitIdleConn) Read(p []byte) (int, error) {
	// Arm only a socket read, never the sync context: local object decoding,
	// checkout, and archiving do not have a wall-clock deadline. Every read
	// (including binary pack data without progress messages) renews the window.
	if err := c.Conn.SetReadDeadline(time.Now().Add(c.idleTimeout)); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(p)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		// go-git can return from a failed pack decode without closing the body.
		// A stalled socket is unusable; close it here to release both endpoints.
		c.Conn.Close()
		return n, fmt.Errorf("%w (no data for %s): %w", errGitIdleTimeout, c.idleTimeout, err)
	}
	return n, err
}

func isGitIdleTimeout(err error) bool {
	// go-git v5.16's DecodeUploadPackResponse formats errors with %s instead
	// of wrapping them. Recognize that path as well as preserved error chains.
	return errors.Is(err, errGitIdleTimeout) || (err != nil && strings.Contains(err.Error(), errGitIdleTimeout.Error()))
}
