package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_InvalidURL(t *testing.T) {
	_, err := New("://bad url")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing base URL")
}

func TestNew_Defaults(t *testing.T) {
	c, err := New("http://example.com")
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, c.http.Timeout)
	assert.NotNil(t, c.http.Transport)
}

func TestWithTimeout(t *testing.T) {
	c, err := New("http://example.com", WithTimeout(5*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, c.http.Timeout)
}

func TestWithTransport(t *testing.T) {
	custom := &http.Transport{MaxIdleConns: 42}
	c, err := New("http://example.com", WithTransport(custom))
	require.NoError(t, err)
	assert.Equal(t, custom, c.http.Transport)
}

func TestURL(t *testing.T) {
	c, err := New("http://example.com/api")
	require.NoError(t, err)

	assert.Equal(t, "http://example.com/api/users", c.URL("/users"))
	assert.Equal(t, "http://example.com/api/users", c.URL("users"))
}

func TestWithMiddleware_Order(t *testing.T) {
	var order []string

	makeMiddleware := func(name string) RoundTripperMiddleware {
		return func(next http.RoundTripper) http.RoundTripper {
			return RoundTripFunc(func(r *http.Request) (*http.Response, error) {
				order = append(order, name)
				return next.RoundTrip(r)
			})
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`"ok"`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithMiddleware(makeMiddleware("first"), makeMiddleware("second")))
	require.NoError(t, err)

	_, _ = Do[string](context.Background(), c, http.MethodGet, "/")
	assert.Equal(t, []string{"first", "second"}, order)
}

func TestWithMiddleware_WrapsTransportRegardlessOfOrder(t *testing.T) {
	called := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`"ok"`))
	}))
	defer srv.Close()

	custom := RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return http.DefaultTransport.RoundTrip(r)
	})

	c, err := New(srv.URL,
		WithMiddleware(func(next http.RoundTripper) http.RoundTripper { return next }),
		WithTransport(custom),
	)
	require.NoError(t, err)

	_, _ = Do[string](context.Background(), c, http.MethodGet, "/")
	assert.True(t, called)
}

func TestDo_Success(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload{Name: "alice"})
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)

	got, err := Do[payload](context.Background(), c, http.MethodGet, "/")
	require.NoError(t, err)
	assert.Equal(t, "alice", got.Name)
}

func TestDo_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)

	_, err = Do[any](context.Background(), c, http.MethodGet, "/")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestDo_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)

	_, err = Do[map[string]any](context.Background(), c, http.MethodGet, "/")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding response")
}

func TestDo_WithHeader(t *testing.T) {
	var receivedToken string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedToken = r.Header.Get("X-Token")
		_, _ = w.Write([]byte(`"ok"`))
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)

	_, err = Do[string](context.Background(), c, http.MethodGet, "/", WithHeader("X-Token", "secret"))
	require.NoError(t, err)
	assert.Equal(t, "secret", receivedToken)
}

func TestDo_WithBody(t *testing.T) {
	var receivedBody, receivedContentType string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(strings.Builder)
		_, _ = io.Copy(buf, r.Body)
		receivedBody = buf.String()
		receivedContentType = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`"ok"`))
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)

	_, err = Do[string](context.Background(), c, http.MethodPost, "/",
		WithBody(strings.NewReader(`{"key":"val"}`), "application/json"),
	)
	require.NoError(t, err)
	assert.Equal(t, `{"key":"val"}`, receivedBody)
	assert.Equal(t, "application/json", receivedContentType)
}

func TestDo_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`"ok"`))
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = Do[string](ctx, c, http.MethodGet, "/")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sending request")
}
