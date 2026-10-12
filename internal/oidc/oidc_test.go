package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var b64 = base64.RawURLEncoding

// Issuer is a fake OIDC provider that signs tokens with its keys.
type Issuer struct {
	*httptest.Server
	rsa      *rsa.PrivateKey
	ec       *ecdsa.PrivateKey
	ed       ed25519.PrivateKey
	jwksHits atomic.Int64
	rsaKid   atomic.Value
}

func newIssuer(t *testing.T) *Issuer {
	is := &Issuer{}
	is.rsa, _ = rsa.GenerateKey(rand.Reader, 2048)
	is.ec, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, is.ed, _ = ed25519.GenerateKey(rand.Reader)
	is.rsaKid.Store("rsa-1")
	is.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]string{"issuer": is.URL, "jwks_uri": is.URL + "/keys"})
		case "/keys":
			is.jwksHits.Add(1)
			w.Write(is.JWKS())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(is.Close)
	return is
}

func (is *Issuer) JWKS() []byte {
	pad := func(b []byte, n int) []byte { return append(make([]byte, n-len(b)), b...) }
	ecPub := is.ec.PublicKey
	b, _ := json.Marshal(map[string]any{"keys": []map[string]string{
		{"kty": "RSA", "kid": is.rsaKid.Load().(string), "use": "sig", "n": b64.EncodeToString(is.rsa.N.Bytes()), "e": b64.EncodeToString(big.NewInt(int64(is.rsa.E)).Bytes())},
		{"kty": "EC", "kid": "ec-1", "crv": "P-256", "x": b64.EncodeToString(pad(ecPub.X.Bytes(), 32)), "y": b64.EncodeToString(pad(ecPub.Y.Bytes(), 32))},
		{"kty": "OKP", "kid": "ed-1", "crv": "Ed25519", "x": b64.EncodeToString(is.ed.Public().(ed25519.PublicKey))},
		{"kty": "RSA", "kid": "enc", "use": "enc", "n": "AQAB", "e": "AQAB"},
	}})
	return b
}

func (is *Issuer) sign(t *testing.T, alg, kid string, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	c, _ := json.Marshal(claims)
	signed := b64.EncodeToString(h) + "." + b64.EncodeToString(c)
	sum := sha256.Sum256([]byte(signed))
	var sig []byte
	var err error
	switch alg {
	case "RS256":
		sig, err = rsa.SignPKCS1v15(rand.Reader, is.rsa, crypto.SHA256, sum[:])
	case "PS256":
		sig, err = rsa.SignPSS(rand.Reader, is.rsa, crypto.SHA256, sum[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	case "ES256":
		r, s, e := ecdsa.Sign(rand.Reader, is.ec, sum[:])
		sig, err = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...), e
	case "EdDSA":
		sig = ed25519.Sign(is.ed, []byte(signed))
	case "HS256": // forged with the RSA modulus as the HMAC secret
		m := hmac.New(sha256.New, is.rsa.N.Bytes())
		m.Write([]byte(signed))
		sig = m.Sum(nil)
	case "none":
	}
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + b64.EncodeToString(sig)
}

func (is *Issuer) claims(extra map[string]any) map[string]any {
	c := map[string]any{"iss": is.URL, "aud": []string{"router", "other"}, "sub": "u1",
		"email": "ada@example.com", "exp": time.Now().Add(time.Hour).Unix()}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

func verifier(is *Issuer) *Verifier {
	return New(Config{Issuer: is.URL, Audiences: []string{"router"}, Leeway: time.Minute}, http.DefaultClient)
}

func TestVerifiesEveryKeyType(t *testing.T) {
	is := newIssuer(t)
	v := verifier(is)
	for alg, kid := range map[string]string{"RS256": "rsa-1", "PS256": "rsa-1", "ES256": "ec-1", "EdDSA": "ed-1"} {
		c, err := v.Verify(context.Background(), is.sign(t, alg, kid, is.claims(nil)))
		if err != nil {
			t.Errorf("%s: %v", alg, err)
			continue
		}
		if c.String("email") != "ada@example.com" {
			t.Errorf("%s: claims %v", alg, c)
		}
	}
	if n := is.jwksHits.Load(); n != 1 {
		t.Errorf("JWKS fetched %d times, want 1", n)
	}
}

func TestRejects(t *testing.T) {
	is := newIssuer(t)
	v := verifier(is)
	ctx := context.Background()
	other := newIssuer(t)
	for name, tc := range map[string]struct {
		token string
		want  error
	}{
		"expired":              {is.sign(t, "RS256", "rsa-1", is.claims(map[string]any{"exp": time.Now().Add(-2 * time.Minute).Unix()})), ErrExpired},
		"not yet valid":        {is.sign(t, "RS256", "rsa-1", is.claims(map[string]any{"nbf": time.Now().Add(time.Hour).Unix()})), ErrInvalid},
		"wrong audience":       {is.sign(t, "RS256", "rsa-1", is.claims(map[string]any{"aud": "someone-else"})), ErrAudience},
		"wrong issuer":         {is.sign(t, "RS256", "rsa-1", is.claims(map[string]any{"iss": "https://evil"})), ErrInvalid},
		"no exp":               {is.sign(t, "RS256", "rsa-1", map[string]any{"iss": is.URL, "aud": "router"}), ErrInvalid},
		"alg none":             {is.sign(t, "none", "rsa-1", is.claims(nil)), ErrInvalid},
		"HMAC with RSA key":    {is.sign(t, "HS256", "rsa-1", is.claims(nil)), ErrInvalid},
		"alg/key mismatch":     {is.sign(t, "ES256", "rsa-1", is.claims(nil)), ErrInvalid},
		"another issuer's key": {other.sign(t, "RS256", "rsa-1", is.claims(nil)), ErrInvalid},
		"tampered":             {tamper(is.sign(t, "RS256", "rsa-1", is.claims(nil))), ErrInvalid},
		"garbage":              {"a.b.c", ErrInvalid},
	} {
		if _, err := v.Verify(ctx, tc.token); !errors.Is(err, tc.want) {
			t.Errorf("%s: err %v, want %v", name, err, tc.want)
		}
	}
}

// tamper swaps the claims for others, keeping the signature.
func tamper(tok string) string {
	parts := strings.Split(tok, ".")
	c, _ := json.Marshal(map[string]any{"sub": "admin"})
	return parts[0] + "." + b64.EncodeToString(c) + "." + parts[2]
}

func TestKeyRotationAndRefetchLimit(t *testing.T) {
	is := newIssuer(t)
	v := verifier(is)
	ctx := context.Background()
	if _, err := v.Verify(ctx, is.sign(t, "RS256", "rsa-1", is.claims(nil))); err != nil {
		t.Fatal(err)
	}
	// The issuer rotates to a new key id; the first token naming it makes
	// the verifier fetch the keys again.
	is.rsaKid.Store("rsa-2")
	clock := time.Now().Add(time.Minute)
	v.now = func() time.Time { return clock }
	if _, err := v.Verify(ctx, is.sign(t, "RS256", "rsa-2", is.claims(map[string]any{"exp": clock.Add(time.Hour).Unix()}))); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	hits := is.jwksHits.Load()
	// Tokens naming unknown keys don't trigger more fetches right away.
	for range 5 {
		v.Verify(ctx, is.sign(t, "RS256", "forged", is.claims(nil)))
	}
	if is.jwksHits.Load() != hits {
		t.Errorf("unknown kids caused %d fetches", is.jwksHits.Load()-hits)
	}
}

func TestLooksLikeJWTAndClaimPaths(t *testing.T) {
	is := newIssuer(t)
	if !LooksLikeJWT(is.sign(t, "RS256", "rsa-1", is.claims(nil))) || LooksLikeJWT("sk-live-abc.def") || LooksLikeJWT("x.y.z") {
		t.Error("LooksLikeJWT")
	}
	c := Claims{"groups": []any{"a", "b"}, "scope": "read write,admin", "realm_access": map[string]any{"roles": []any{"ml"}},
		"https://example.com/team": "core"}
	if got := strings.Join(c.Strings("groups"), ","); got != "a,b" {
		t.Errorf("groups %q", got)
	}
	if got := strings.Join(c.Strings("scope"), ","); got != "read,write,admin" {
		t.Errorf("scope %q", got)
	}
	if got := c.Strings("realm_access.roles"); len(got) != 1 || got[0] != "ml" {
		t.Errorf("nested %q", got)
	}
	if c.String("https://example.com/team") != "core" || c.String("missing") != "" {
		t.Error("String")
	}
}

func TestJWKSFileAndWeakKeys(t *testing.T) {
	is := newIssuer(t)
	keys, err := ParseJWKS(is.JWKS())
	if err != nil || len(keys) != 3 {
		t.Fatalf("keys %d, %v (the enc key must be skipped)", len(keys), err)
	}
	weak, _ := rsa.GenerateKey(rand.Reader, 1024)
	raw, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "w", "n": b64.EncodeToString(weak.N.Bytes()), "e": "AQAB"}}})
	if _, err := ParseJWKS(raw); err == nil {
		t.Error("a 1024-bit RSA key was accepted")
	}
}
