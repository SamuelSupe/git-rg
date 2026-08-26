package provider

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientRequestBudgetCountsRetries(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "temporary")
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	client := newClient(Options{Timeout: 2 * time.Second, RequestLimit: 2}, func(*http.Request) {})
	response, err := client.get(context.Background(), server.URL, "")
	if err != nil {
		t.Fatalf("get() error = %v", err)
	}
	response.Body.Close()
	stats := client.stats()
	if attempts.Load() != 2 || stats.Requests != 2 || stats.Retries != 1 || stats.RequestLimit != 2 {
		t.Fatalf("attempts/stats = %d/%#v, want 2 requests and 1 retry", attempts.Load(), stats)
	}
}

func TestClientRequestBudgetCountsRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		_, _ = io.WriteString(w, "target")
	}))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	client := newTLSClient(Options{Timeout: time.Second, RequestLimit: 1}, func(*http.Request) {})
	_, err := client.get(context.Background(), source.URL, "")
	var budgetErr *RequestBudgetError
	if err == nil || !errors.As(err, &budgetErr) {
		t.Fatalf("get() error = %v, want request budget error", err)
	}
	if targetHits.Load() != 0 || client.stats().Requests != 1 {
		t.Fatalf("target hits/stats = %d/%#v, want redirect blocked after one request", targetHits.Load(), client.stats())
	}
}

func TestClientCapturesRateLimitHeadersInStats(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "77")
		w.Header().Set("X-RateLimit-Remaining", "66")
		w.Header().Set("X-RateLimit-Reset", "1234567890")
		w.Header().Set("X-RateLimit-Resource", "core")
		w.Header().Set("RateLimit-Name", "github")
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	client := newClient(Options{Timeout: time.Second}, func(*http.Request) {})
	response, err := client.get(context.Background(), server.URL, "")
	if err != nil {
		t.Fatalf("get() error = %v", err)
	}
	response.Body.Close()
	stats := client.stats()
	if stats.RateLimit == nil {
		t.Fatal("RequestStats().RateLimit is nil")
	}
	want := &RateLimit{Limit: 77, Remaining: 66, Reset: 1234567890, Resource: "core", Name: "github"}
	if *stats.RateLimit != *want {
		t.Fatalf("RateLimit = %#v, want %#v", *stats.RateLimit, *want)
	}
}

func TestClientRejectsTrailingJSONResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true} {"unexpected":true}`)
	}))
	defer server.Close()
	client := newClient(Options{Timeout: time.Second}, func(*http.Request) {})
	var response map[string]bool
	if _, err := client.getJSON(context.Background(), server.URL, &response); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("getJSON() error = %v, want trailing JSON rejection", err)
	}
}

func TestClientClassifiesForbiddenRetryAfterWithoutWaiting(t *testing.T) {
	for _, retryAfter := range []string{"31", "999999999999999999999999999999999999999999999999"} {
		t.Run(retryAfter, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", retryAfter)
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"message":"secondary rate limit"}`)
			}))
			defer server.Close()

			client := newClient(Options{Timeout: time.Second}, func(*http.Request) {})
			started := time.Now()
			_, err := client.get(context.Background(), server.URL, "")
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("get() waited %s for Retry-After %q", elapsed, retryAfter)
			}
			var httpErr *HTTPError
			if err == nil || !errors.As(err, &httpErr) {
				t.Fatalf("get() error = %v, want HTTPError", err)
			}
			if httpErr.Status != http.StatusForbidden || !httpErr.RateLimited || httpErr.RetryAfter <= maxRetryDelay {
				t.Fatalf("HTTPError = %#v, want rate-limited Retry-After above retry delay", httpErr)
			}
			if got := requests.Load(); got != 1 || client.stats().Requests != 1 {
				t.Fatalf("requests/stats = %d/%#v, want one request without retry", got, client.stats())
			}
		})
	}
}

func TestClientRejectsOversizedJSONResponsesAsResourceLimit(t *testing.T) {
	t.Run("content length", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", strconv.FormatInt(maxJSONResponseSize+1, 10))
			_, _ = io.WriteString(w, `{}`)
		}))
		defer server.Close()

		client := newClient(Options{Timeout: time.Second}, func(*http.Request) {})
		var response map[string]any
		_, err := client.getJSON(context.Background(), server.URL, &response)
		var limitErr *ResourceLimitError
		if err == nil || !errors.As(err, &limitErr) {
			t.Fatalf("getJSON() error = %v, want ResourceLimitError", err)
		}
	})

	t.Run("streamed body", func(t *testing.T) {
		client := newClient(Options{Timeout: time.Second}, func(*http.Request) {})
		client.http.Transport = responseRoundTripper(func(*http.Request) (*http.Response, error) {
			prefix := []byte(`{"ok":true}`)
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        make(http.Header),
				Body:          io.NopCloser(&jsonPaddingReader{prefix: prefix, remaining: maxJSONResponseSize + 1 - int64(len(prefix))}),
				ContentLength: -1,
			}, nil
		})
		var response map[string]any
		_, err := client.getJSON(context.Background(), "http://provider.test/oversized", &response)
		var limitErr *ResourceLimitError
		if err == nil || !errors.As(err, &limitErr) {
			t.Fatalf("getJSON() error = %v, want ResourceLimitError", err)
		}
	})
}

func TestClientCrossHostHTTPSRedirectStripsSecrets(t *testing.T) {
	var authorization, privateToken, cookie string
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		privateToken = r.Header.Get("Private-Token")
		cookie = r.Header.Get("Cookie")
		_, _ = io.WriteString(w, "target")
	}))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	client := newTLSClient(Options{Timeout: time.Second}, secretHeaders)
	response, err := client.get(context.Background(), source.URL, "")
	if err != nil {
		t.Fatalf("cross-host HTTPS redirect error = %v", err)
	}
	response.Body.Close()
	if authorization != "" || privateToken != "" || cookie != "" {
		t.Fatalf("redirect leaked secrets: Authorization=%q Private-Token=%q Cookie=%q", authorization, privateToken, cookie)
	}
}

func TestClientSameHostRedirectKeepsSecrets(t *testing.T) {
	var authorization, privateToken, cookie string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		authorization = r.Header.Get("Authorization")
		privateToken = r.Header.Get("Private-Token")
		cookie = r.Header.Get("Cookie")
		_, _ = io.WriteString(w, "same-host")
	}))
	defer server.Close()

	client := newTLSClient(Options{Timeout: time.Second}, secretHeaders)
	response, err := client.get(context.Background(), server.URL, "")
	if err != nil {
		t.Fatalf("same-host redirect error = %v", err)
	}
	response.Body.Close()
	if authorization != "Bearer secret" || privateToken != "gitlab-secret" || cookie != "session=secret" {
		t.Fatalf("same-host headers = %q/%q/%q", authorization, privateToken, cookie)
	}
}

func TestClientRejectsHTTPSDowngradeRedirect(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		_, _ = io.WriteString(w, "downgrade")
	}))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()

	client := newTLSClient(Options{Timeout: time.Second}, secretHeaders)
	_, err := client.get(context.Background(), source.URL, "")
	if err == nil || !strings.Contains(err.Error(), "HTTPS downgrade") {
		t.Fatalf("downgrade error = %v, want HTTPS downgrade refusal", err)
	}
	if targetHits.Load() != 0 {
		t.Fatalf("downgrade target was contacted %d times", targetHits.Load())
	}
}

func secretHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Private-Token", "gitlab-secret")
	req.Header.Set("Cookie", "session=secret")
}

func newTLSClient(options Options, headers func(*http.Request)) *client {
	client := newClient(options, headers, "secret", "gitlab-secret")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // test servers use self-signed certificates.
	client.http.Transport = transport
	return client
}

type responseRoundTripper func(*http.Request) (*http.Response, error)

func (f responseRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type jsonPaddingReader struct {
	prefix    []byte
	position  int
	remaining int64
}

func (r *jsonPaddingReader) Read(buffer []byte) (int, error) {
	if r.position < len(r.prefix) {
		read := copy(buffer, r.prefix[r.position:])
		r.position += read
		return read, nil
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	read := len(buffer)
	if int64(read) > r.remaining {
		read = int(r.remaining)
	}
	for index := 0; index < read; index++ {
		buffer[index] = ' '
	}
	r.remaining -= int64(read)
	return read, nil
}
