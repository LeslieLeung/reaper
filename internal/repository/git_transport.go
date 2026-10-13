package repository

import (
	"fmt"
	"net/http"

	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// This private auth type selects the guarded transport only for credentials
// provided by our GitHub account pool. Other callers retain their transport.
type githubGitAuth struct{ *githttp.BasicAuth }

func init() {
	// Register once before concurrent syncs, never while operations are running.
	gitclient.InstallProtocol("https", &githubGitTransport{base: gitclient.Protocols["https"]})
}

type githubGitTransport struct{ base transport.Transport }

func (t *githubGitTransport) client(auth transport.AuthMethod) transport.Transport {
	if _, ok := auth.(*githubGitAuth); !ok {
		return t.base
	}
	return githttp.NewClient(&http.Client{
		Transport: githubCredentialTransport{base: http.DefaultTransport},
	})
}

func (t *githubGitTransport) NewUploadPackSession(endpoint *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	return t.client(auth).NewUploadPackSession(endpoint, auth)
}

func (t *githubGitTransport) NewReceivePackSession(endpoint *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	return t.client(auth).NewReceivePackSession(endpoint, auth)
}

type githubCredentialTransport struct{ base http.RoundTripper }

func (t githubCredentialTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	// go-git can change its pack endpoint after info/refs redirects. Check every
	// outgoing request, including redirects with copied Authorization headers.
	if !gitHubHTTPSURL(request.URL.String()) {
		return nil, fmt.Errorf("%w: refusing GitHub credentials outside github.com HTTPS", transport.ErrAuthorizationFailed)
	}
	return t.base.RoundTrip(request)
}
