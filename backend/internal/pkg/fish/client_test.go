package fish

import (
	"context"
	"errors"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient(Options{APIBase: server.URL})
	return client, server
}

func decodeJSONBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body %q: %v", raw, err)
	}
	return body
}

func TestBrowserHeaderSet(t *testing.T) {
	var got http.Header
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credits":1,"pricingVersion":"v"}`))
	}))
	_, err := client.Price(context.Background(), "tok", Identity{DeviceID: "dev-1", TeamID: "team-1"}, PriceParams{
		Model: "gpt-image-2", Variant: VariantTextToImage, ImageSize: "1024x1024", Count: 1,
	})
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	checks := map[string]string{
		"Authorization":       "Bearer tok",
		"Accept":              "application/json",
		"Origin":              WebOrigin,
		"User-Agent":          chromeUserAgent,
		"Sec-Ch-Ua":           chromeSecCHUA,
		"Sec-Ch-Ua-Mobile":    "?0",
		"Sec-Ch-Ua-Platform":  chromeSecCHUAPlat,
		"Sec-Fetch-Dest":      "empty",
		"Sec-Fetch-Mode":      "cors",
		"Sec-Fetch-Site":      "same-site",
		"X-Fish-Amp-Device-Id": "dev-1",
		"X-Team-Id":           "team-1",
		"X-Workspace-Id":      "team-1",
	}
	for header, want := range checks {
		if got := got.Get(header); got != want {
			t.Errorf("header %s = %q, want %q", header, got, want)
		}
	}
	for _, absent := range []string{"Referer", "Cookie", "Dnt"} {
		if v := got.Get(absent); v != "" {
			t.Errorf("header %s should be absent, got %q", absent, v)
		}
	}
	sid := got.Get("X-Fish-Amp-Session-Id")
	if sid == "" || strings.TrimLeft(sid, "0123456789") != "" {
		t.Errorf("X-Fish-Amp-Session-Id = %q, want ms timestamp digits", sid)
	}
}

func TestErrorEnvelopeClassification(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		message string
		check   func(error) bool
	}{
		{"auth 401", 401, "Invalid token", func(err error) bool { _, ok := err.(*AuthError); return ok }},
		{"rate limited 429", 429, "Too many requests", func(err error) bool { _, ok := err.(*RateLimitedError); return ok }},
		{"credits 402", 402, "insufficient credits", func(err error) bool { _, ok := err.(*InsufficientCreditsError); return ok }},
		{"credits by message", 400, "You have exceeded your daily image limit", func(err error) bool { _, ok := err.(*InsufficientCreditsError); return ok }},
		{"forbidden 403", 403, `GENERATION_UNAVAILABLE: {"code":"GENERATION_UNAVAILABLE"}`, func(err error) bool { _, ok := err.(*ForbiddenError); return ok }},
		{"invalid combination", 400, "Pricing configuration must match exactly one rate: matches=0", func(err error) bool { _, ok := err.(*InvalidCombinationError); return ok }},
		{"upstream 5xx", 503, "unavailable", func(err error) bool { _, ok := err.(*UpstreamTemporaryError); return ok }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": tc.status, "message": tc.message})
			}))
			_, err := client.Price(context.Background(), "tok", Identity{}, PriceParams{Model: "m", ImageSize: "1K", Count: 1})
			if err == nil {
				t.Fatal("expected error")
			}
			if !tc.check(err) {
				t.Fatalf("error type mismatch: %v", err)
			}
		})
	}
}

func TestSessionIDRotation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	client := NewClient(Options{Now: func() time.Time { return now }})
	first := client.sessionIDFor(Identity{})
	if first == "" {
		t.Fatal("session id empty")
	}
	// 窗口内复用
	if second := client.sessionIDFor(Identity{}); second != first {
		t.Fatalf("session id rotated within idle window: %q vs %q", second, first)
	}
	// 闲置超窗口后轮换
	now = now.Add(sessionIdleRotation + time.Second)
	if third := client.sessionIDFor(Identity{}); third == first {
		t.Fatalf("session id did not rotate after idle window: %q", third)
	}
	// Identity 显式携带则恒定透传
	if explicit := client.sessionIDFor(Identity{SessionID: "fixed"}); explicit != "fixed" {
		t.Fatalf("explicit session id not honored: %q", explicit)
	}
}

func TestDownloadImage(t *testing.T) {
	payload := []byte("fake-png-bytes")
	var requestPath string
	client, server := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		_, _ = w.Write(payload)
	}))
	got, err := client.DownloadImage(context.Background(), server.URL+"/img.png")
	if err != nil {
		t.Fatalf("DownloadImage: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("downloaded bytes mismatch")
	}
	if requestPath != "/img.png" {
		t.Fatalf("unexpected path %q", requestPath)
	}
}

func TestOutOfCreditsEnvelopeCarriesCode(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"status":402,"code":"OUT_OF_CREDITS","message":"Out of credits","ctaKind":"upgrade_plan"}`))
	}))
	_, err := client.Price(context.Background(), "tok", Identity{}, PriceParams{Model: "gpt-image-2", ImageSize: "1024x1024", Count: 1})
	var insufficient *InsufficientCreditsError
	if !errors.As(err, &insufficient) {
		t.Fatalf("err = %v, want InsufficientCreditsError", err)
	}
	if !strings.Contains(err.Error(), "OUT_OF_CREDITS") {
		t.Fatalf("error message must carry stable code, got %q", err.Error())
	}
}

func TestForbiddenEnvelopeCarriesNativeBody(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"code":"GENERATION_UNAVAILABLE"}`))
	}))
	_, err := client.Price(context.Background(), "tok", Identity{}, PriceParams{Model: "gpt-image-2", ImageSize: "1024x1024", Count: 1})
	var forbidden *ForbiddenError
	if !errors.As(err, &forbidden) {
		t.Fatalf("err = %v, want ForbiddenError", err)
	}
	// 账号置 error 的文案逐字取这条原生串，不得改写
	want := `fish: GENERATION_UNAVAILABLE: {"code":"GENERATION_UNAVAILABLE"} (status 403)`
	if err.Error() != want {
		t.Fatalf("error string = %q, want %q", err.Error(), want)
	}
}
