package catalog

import (
	"context"
	"net/http"
)

type httpClientKey struct{}

// WithHTTPClient binds model-list and endpoint-probe requests to a caller's
// client, so an administrative draft can impose its own network boundaries.
func WithHTTPClient(ctx context.Context, client *http.Client) context.Context {
	return context.WithValue(ctx, httpClientKey{}, client)
}

// HTTPClient returns the client selected for this operation, or the usual one.
func HTTPClient(ctx context.Context) *http.Client {
	if client, ok := ctx.Value(httpClientKey{}).(*http.Client); ok && client != nil {
		return client
	}
	return http.DefaultClient
}
