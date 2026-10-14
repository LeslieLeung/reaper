package repository

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	gitHTTP "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/stretchr/testify/require"
)

func TestGitAccountTransportDetectsIdle(t *testing.T) {
	seenToken := make(chan string, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, token, _ := r.BasicAuth()
		seenToken <- token
		<-r.Context().Done()
	}))
	defer server.Close()
	idleClient := newGitHTTPClient(100 * time.Millisecond)
	defer idleClient.CloseIdleConnections()
	tr := idleClient.Transport.(*http.Transport)
	// Route a trusted GitHub endpoint to the controlled TLS test server.
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dial(ctx, network, server.Listener.Addr().String())
	}
	guarded := &githubGitTransport{base: gitHTTP.NewClient(idleClient), httpBase: tr}
	endpoint, err := transport.NewEndpoint("https://github.com/owner/repo")
	require.NoError(t, err)
	session, err := guarded.NewUploadPackSession(endpoint, &githubGitAuth{BasicAuth: &gitHTTP.BasicAuth{Username: "x-access-token", Password: "test-token"}})
	require.NoError(t, err)
	defer session.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = session.AdvertisedReferencesContext(ctx)
	require.ErrorIs(t, err, errGitIdleTimeout)
	require.NoError(t, ctx.Err())
	require.Equal(t, "test-token", <-seenToken)
}

func TestGitHTTPClientTimesOutWithoutResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	client := newGitHTTPClient(100 * time.Millisecond)
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	_, err = client.Do(req)
	require.ErrorIs(t, err, errGitIdleTimeout)
	require.NoError(t, ctx.Err(), "idle detection must fire before the caller's deadline")
}

func TestGitHTTPClientTimesOutWhenResponseBodyStalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("first chunk"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := newGitHTTPClient(100 * time.Millisecond)
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.Equal(t, "first chunk", string(data))
	require.ErrorIs(t, err, errGitIdleTimeout)
	require.NoError(t, ctx.Err())
}

func TestGitHTTPClientConcurrentTrafficDoesNotHideStall(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data"))
		w.(http.Flusher).Flush()
		if r.URL.Path == "/stalled" {
			<-r.Context().Done()
			return
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
				if _, err := w.Write([]byte("data")); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	gitClient := newGitHTTPClient(150 * time.Millisecond)
	defer gitClient.CloseIdleConnections()
	gitClient.Transport.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	activeReq, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/active", nil)
	require.NoError(t, err)
	activeResp, err := gitClient.Do(activeReq)
	require.NoError(t, err)
	defer activeResp.Body.Close()
	finished := make(chan struct{})
	go func() {
		io.Copy(io.Discard, activeResp.Body)
		close(finished)
	}()
	defer func() {
		cancel()
		<-finished
	}()
	stalledReq, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/stalled", nil)
	require.NoError(t, err)
	stalledResp, err := gitClient.Do(stalledReq)
	require.NoError(t, err)
	defer stalledResp.Body.Close()
	_, err = io.ReadAll(stalledResp.Body)
	require.ErrorIs(t, err, errGitIdleTimeout)
	require.NoError(t, ctx.Err(), "other transfers must not renew a stalled transfer's idle window")
}

func TestGitHTTPClientKeepsReceivingBeyondIdleTimeout(t *testing.T) {
	const idleTimeout = 300 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 10; i++ {
			if _, err := w.Write([]byte("data")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}))
	defer server.Close()
	client := newGitHTTPClient(idleTimeout)
	defer client.CloseIdleConnections()
	start := time.Now()
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "datadatadatadatadatadatadatadatadatadata", string(data))
	require.Greater(t, time.Since(start), idleTimeout)
}

func TestGitHTTPClientRespectsCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	client := newGitHTTPClient(time.Minute)
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	go func() {
		<-started
		cancel()
	}()
	_, err = client.Do(req)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, errGitIdleTimeout)
}

func TestGitHTTPClientTLSBodyStalls(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		name := "HTTP1Server"
		if http2 {
			name = "HTTP2CapableServer"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("pack bytes"))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			server.EnableHTTP2 = http2
			server.StartTLS()
			defer server.Close()
			gitClient := newGitHTTPClient(100 * time.Millisecond)
			defer gitClient.CloseIdleConnections()
			gitClient.Transport.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			require.NoError(t, err)
			resp, err := gitClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			require.Equal(t, "pack bytes", string(data))
			require.ErrorIs(t, err, errGitIdleTimeout)
			require.NoError(t, ctx.Err())
		})
	}
}

func TestGitIdleConnLocalProcessingDoesNotExpireNextRead(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	conn := &gitIdleConn{Conn: local, idleTimeout: 100 * time.Millisecond}
	writeErrors := make(chan error, 1)
	go func() {
		_, err := remote.Write([]byte("a"))
		writeErrors <- err
	}()
	var data [1]byte
	_, err := io.ReadFull(conn, data[:])
	require.NoError(t, err)
	require.Equal(t, byte('a'), data[0])
	require.NoError(t, <-writeErrors)
	// No socket reads while the caller processes decoded objects locally.
	time.Sleep(200 * time.Millisecond)
	go func() {
		_, err := remote.Write([]byte("b"))
		writeErrors <- err
	}()
	_, err = io.ReadFull(conn, data[:])
	require.NoError(t, err, "the next network read must get a fresh idle window")
	require.Equal(t, byte('b'), data[0])
	require.NoError(t, <-writeErrors)
}

func TestCloneFailsWhenGitPackStreamStalls(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			refs := packp.NewAdvRefs()
			refs.Prefix = [][]byte{[]byte("# service=git-upload-pack\n"), {}}
			hash := plumbing.NewHash("1111111111111111111111111111111111111111")
			refs.Head = &hash
			refs.References["refs/heads/master"] = hash
			refs.Capabilities.Set(capability.SymRef, "HEAD:refs/heads/master")
			if err := refs.Encode(w); err != nil {
				t.Error(err)
			}
			return
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		// Valid negotiation response, then a partial binary pack header with no
		// progress output. go-git must detect a stalled pack read, not just logs.
		w.Write([]byte("0008NAK\nPACK"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	gitClient := newGitHTTPClient(100 * time.Millisecond)
	defer gitClient.CloseIdleConnections()
	gitClient.Transport.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	previous := client.Protocols["https"]
	client.InstallProtocol("https", gitHTTP.NewClient(gitClient))
	defer client.InstallProtocol("https", previous)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := git.PlainCloneContext(ctx, t.TempDir(), false, &git.CloneOptions{URL: server.URL + "/owner/repo"})
	require.True(t, isGitIdleTimeout(err), "go-git must report the network stall: %v", err)
	require.NoError(t, ctx.Err())
}
