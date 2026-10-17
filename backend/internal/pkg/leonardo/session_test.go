package leonardo

import (
	"net/http"
	"testing"
	"time"
)

func TestNormalizeCookie(t *testing.T) {
	cases := map[string]string{
		"raw-token-value":                      SessionTokenCookieName + "=raw-token-value",
		"k1=v1; k2=v2":                         "k1=v1; k2=v2",
		"single=pair":                          "single=pair",
		"":                                     "",
		SessionTokenCookieName + "=x; other=y": SessionTokenCookieName + "=x; other=y",
	}
	for input, want := range cases {
		if got := NormalizeCookie(input); got != want {
			t.Fatalf("NormalizeCookie(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestExtractSessionToken(t *testing.T) {
	cookie := "cf_clearance=abc; " + SessionTokenCookieName + "=THE-TOKEN; " + SessionDataCookiePfx + "0=shard"
	if got := ExtractSessionToken(cookie); got != "THE-TOKEN" {
		t.Fatalf("ExtractSessionToken = %q", got)
	}
	if got := ExtractSessionToken("nothing-here"); got != "" {
		t.Fatalf("ExtractSessionToken should be empty, got %q", got)
	}
}

func TestMergeResponseCookiesRotation(t *testing.T) {
	existing := SessionTokenCookieName + "=old; other=keep"
	rotated := &http.Cookie{Name: SessionTokenCookieName, Value: "new"}
	merged := mergeResponseCookies(existing, []*http.Cookie{rotated})
	want := SessionTokenCookieName + "=new; other=keep"
	if merged != want {
		t.Fatalf("rotation merge = %q, want %q", merged, want)
	}
	// 分片新增。
	shard := &http.Cookie{Name: SessionDataCookiePfx + "0", Value: "s0"}
	merged = mergeResponseCookies(merged, []*http.Cookie{shard})
	if merged != SessionTokenCookieName+"=new; other=keep; "+SessionDataCookiePfx+"0=s0" {
		t.Fatalf("shard merge = %q", merged)
	}
}

func TestMergeResponseCookiesDeletion(t *testing.T) {
	existing := SessionTokenCookieName + "=v; other=keep"
	deletion := &http.Cookie{Name: SessionTokenCookieName, Value: "", MaxAge: -1}
	merged := mergeResponseCookies(existing, []*http.Cookie{deletion})
	if merged != "other=keep" {
		t.Fatalf("deletion merge = %q", merged)
	}
	if !deletesCriticalSessionCookie([]*http.Cookie{deletion}) {
		t.Fatal("critical deletion should be detected")
	}
	if deletesCriticalSessionCookie([]*http.Cookie{{Name: "other", Value: "x", MaxAge: -1}}) {
		t.Fatal("non-critical deletion should not be flagged")
	}
	// 同名先删后设：最终动作是设置，不算删除。
	setAgain := &http.Cookie{Name: SessionTokenCookieName, Value: "fresh", Expires: time.Now().Add(time.Hour)}
	if deletesCriticalSessionCookie([]*http.Cookie{deletion, setAgain}) {
		t.Fatal("last action wins: set-after-delete must not count as deletion")
	}
}

func TestParseJWT(t *testing.T) {
	// header.payload.signature —— payload 是 {"sub":"sub-1","email":"a@b.c","exp":1800000000}
	token := "eyJhbGciOiJub25lIn0.eyJzdWIiOiJzdWItMSIsImVtYWlsIjoiYUBiLmMiLCJleHAiOjE4MDAwMDAwMDB9.sig"
	expiresAt, sub, email, err := ParseJWT(token)
	if err != nil {
		t.Fatalf("ParseJWT error: %v", err)
	}
	if sub != "sub-1" || email != "a@b.c" {
		t.Fatalf("sub/email = %q/%q", sub, email)
	}
	if expiresAt.Unix() != 1800000000 {
		t.Fatalf("exp = %d", expiresAt.Unix())
	}
	if _, _, _, err := ParseJWT("not-a-jwt"); err == nil {
		t.Fatal("ParseJWT should reject malformed token")
	}
}

func TestExtractJWT(t *testing.T) {
	jwt := "aaa.bbb.ccc"
	cases := []struct {
		name string
		data map[string]any
	}{
		{"session.token", map[string]any{"session": map[string]any{"token": jwt}}},
		{"session.idToken", map[string]any{"session": map[string]any{"idToken": jwt}}},
		{"top-level token", map[string]any{"token": jwt}},
		{"user.token", map[string]any{"user": map[string]any{"token": jwt}}},
	}
	for _, tc := range cases {
		if got := extractJWT(tc.data); got != jwt {
			t.Fatalf("%s: extractJWT = %q", tc.name, got)
		}
	}
	if got := extractJWT(map[string]any{"session": map[string]any{"token": "no-dots"}}); got != "" {
		t.Fatalf("non-JWT value should be rejected, got %q", got)
	}
}
