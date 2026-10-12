package githubapi

import "github.com/google/go-github/v56/github"

// NewREST authenticates a fresh REST client without mutating the shared base
// HTTP transport (go-github/v56 WithAuthToken modifies its HTTP client).
// The unauthenticated base owns endpoint, transport, and user agent test seams.
func NewREST(base *github.Client, token string) *github.Client {
	httpCopy := *base.Client()
	client := github.NewClient(&httpCopy)
	client.BaseURL, client.UploadURL = base.BaseURL, base.UploadURL
	client.UserAgent = base.UserAgent
	if token != "" {
		client = client.WithAuthToken(token)
	}
	return client
}
