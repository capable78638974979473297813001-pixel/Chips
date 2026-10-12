// Package oidctest is a fake OpenID Connect provider for tests: it serves
// discovery and a JWKS, and signs RS256 tokens.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type Issuer struct {
	*httptest.Server
	key *rsa.PrivateKey
}

func New(t testing.TB) *Issuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &Issuer{key: key}
	b64 := base64.RawURLEncoding
	is.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]string{"issuer": is.URL, "jwks_uri": is.URL + "/keys"})
		case "/keys":
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
				"n": b64.EncodeToString(key.N.Bytes()), "e": b64.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(is.Close)
	return is
}

// Token signs a token for audience aud with these claims added to iss,
// aud, and an exp an hour from now.
func (is *Issuer) Token(t testing.TB, aud string, claims map[string]any) string {
	c := map[string]any{"iss": is.URL, "aud": aud, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}
	for k, v := range claims {
		c[k] = v
	}
	b64 := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	body, _ := json.Marshal(c)
	signed := b64.EncodeToString(h) + "." + b64.EncodeToString(body)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, is.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + b64.EncodeToString(sig)
}
