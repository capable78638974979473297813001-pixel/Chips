// Package oidc verifies JWT access tokens from an OpenID Connect provider
// (Okta, Entra ID, Google, Keycloak, Auth0, Dex, ...) so people can use the
// router with their company login instead of shared API keys.
//
// Tokens are checked against the issuer's published signing keys (JWKS),
// which are fetched through OIDC discovery, cached, and re-fetched when a
// token names a key not seen yet (key rotation). Only asymmetric
// algorithms are accepted: RS256/384/512, PS256/384/512, ES256/384/512 and
// EdDSA. "none" and HMAC algorithms are refused, since an HMAC "key" from a
// JWKS would let anyone who can read the JWKS mint tokens.
package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// Config is what Verifier needs.
type Config struct {
	// Issuer is the provider's issuer URL; tokens must carry it as iss,
	// and its discovery document names the JWKS (unless JWKSURL is set).
	Issuer string
	// Audiences: a token's aud must include one of these.
	Audiences []string
	JWKSURL   string // skips discovery
	// JWKSFile reads keys from a file instead of the network (air-gapped
	// installs); it is re-read on refresh.
	JWKSFile string
	Leeway   time.Duration // clock skew allowed on exp and nbf
	// Refresh is how often keys are re-fetched even when no token needs a
	// new one.
	Refresh time.Duration
}

// Claims are a verified token's claims.
type Claims map[string]any

// Verifier checks tokens. Safe for concurrent use.
type Verifier struct {
	cfg    Config
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	keys      map[string]crypto.PublicKey // by kid ("" for keys without one)
	fetched   time.Time
	lastTry   time.Time
	jwksURL   string
	fetchErr  error
	fetchLock sync.Mutex // one fetch at a time

	cacheMu sync.Mutex
	cache   map[[32]byte]cached
}

type cached struct {
	claims Claims
	until  time.Time
}

// cacheSize bounds verified tokens kept to skip signature checks; a
// client sends the same token for its every request until it expires.
const cacheSize = 10000

// minRefetch spaces out JWKS fetches triggered by unknown key ids, so a
// flood of forged tokens cannot turn into a flood of fetches.
const minRefetch = 30 * time.Second

func New(cfg Config, client *http.Client) *Verifier {
	if cfg.Refresh == 0 {
		cfg.Refresh = time.Hour
	}
	return &Verifier{cfg: cfg, client: client, now: time.Now, cache: map[[32]byte]cached{}}
}

// LooksLikeJWT reports whether s has a JWT's shape (three dot-separated
// base64url parts, the first decoding to a JSON header). API keys don't.
func LooksLikeJWT(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 || len(parts[0]) < 10 {
		return false
	}
	h, err := base64.RawURLEncoding.DecodeString(parts[0])
	return err == nil && len(h) > 0 && h[0] == '{'
}

// Errors a caller may want to tell apart (all wrapped in the returned error).
var (
	ErrExpired  = errors.New("token expired")
	ErrInvalid  = errors.New("invalid token")
	ErrNoKeys   = errors.New("signing keys unavailable")
	ErrAudience = errors.New("token not issued for this audience")
)

// Verify checks token's signature, issuer, audience and validity window
// and returns its claims.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	now := v.now()
	sum := sha256.Sum256([]byte(token))
	v.cacheMu.Lock()
	if c, ok := v.cache[sum]; ok && now.Before(c.until) {
		v.cacheMu.Unlock()
		return c.claims, nil
	}
	v.cacheMu.Unlock()

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: not a JWT", ErrInvalid)
	}
	var header struct {
		Alg  string   `json:"alg"`
		Kid  string   `json:"kid"`
		Crit []string `json:"crit"`
	}
	if err := decodePart(parts[0], &header); err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrInvalid, err)
	}
	if len(header.Crit) > 0 {
		return nil, fmt.Errorf("%w: critical header extensions are not supported", ErrInvalid)
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature encoding", ErrInvalid)
	}
	if err := verifySig(header.Alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var claims Claims
	if err := decodePart(parts[1], &claims); err != nil {
		return nil, fmt.Errorf("%w: claims: %v", ErrInvalid, err)
	}
	until, err := v.checkClaims(claims, now)
	if err != nil {
		return nil, err
	}
	v.cacheMu.Lock()
	if len(v.cache) >= cacheSize {
		clear(v.cache)
	}
	v.cache[sum] = cached{claims, until}
	v.cacheMu.Unlock()
	return claims, nil
}

func decodePart(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// checkClaims validates iss, aud, exp and nbf, and returns when the token
// stops being valid.
func (v *Verifier) checkClaims(c Claims, now time.Time) (time.Time, error) {
	if v.cfg.Issuer != "" {
		if iss, _ := c["iss"].(string); iss != v.cfg.Issuer {
			return time.Time{}, fmt.Errorf("%w: issuer %q, want %q", ErrInvalid, iss, v.cfg.Issuer)
		}
	}
	if len(v.cfg.Audiences) > 0 {
		var auds []string
		switch a := c["aud"].(type) {
		case string:
			auds = []string{a}
		case []any:
			for _, x := range a {
				if s, ok := x.(string); ok {
					auds = append(auds, s)
				}
			}
		}
		if !slices.ContainsFunc(auds, func(a string) bool { return slices.Contains(v.cfg.Audiences, a) }) {
			return time.Time{}, ErrAudience
		}
	}
	exp, ok := numericDate(c["exp"])
	if !ok {
		return time.Time{}, fmt.Errorf("%w: no exp claim", ErrInvalid)
	}
	if !now.Before(exp.Add(v.cfg.Leeway)) {
		return time.Time{}, ErrExpired
	}
	if nbf, ok := numericDate(c["nbf"]); ok && now.Add(v.cfg.Leeway).Before(nbf) {
		return time.Time{}, fmt.Errorf("%w: not valid yet", ErrInvalid)
	}
	return exp.Add(v.cfg.Leeway), nil
}

func numericDate(v any) (time.Time, bool) {
	f, ok := v.(float64)
	if !ok {
		return time.Time{}, false
	}
	sec := int64(f)
	return time.Unix(sec, int64((f-float64(sec))*1e9)), true
}

// key returns the signing key kid names, fetching the JWKS when it is not
// known yet (at most every minRefetch) or the keys are older than Refresh.
func (v *Verifier) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	v.mu.Lock()
	k, ok := v.keys[kid]
	stale := v.now().Sub(v.fetched) > v.cfg.Refresh
	v.mu.Unlock()
	if ok && !stale {
		return k, nil
	}
	// On demand fetches are rate-limited either way: an issuer that is down
	// must not cost every request a fetch while the old keys still work.
	if err := v.refresh(ctx, true); err != nil && !ok {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	if kid == "" && len(v.keys) == 1 { // a token without kid, an issuer with one key
		for _, k := range v.keys {
			return k, nil
		}
	}
	return nil, fmt.Errorf("%w: no signing key %q", ErrInvalid, kid)
}

// Refresh fetches the signing keys now. Call it at startup to find
// configuration mistakes early; Verify fetches them on demand anyway.
func (v *Verifier) Refresh(ctx context.Context) error { return v.refresh(ctx, false) }

func (v *Verifier) refresh(ctx context.Context, limited bool) error {
	v.fetchLock.Lock()
	defer v.fetchLock.Unlock()
	v.mu.Lock()
	if limited && v.now().Sub(v.lastTry) < minRefetch {
		err := v.fetchErr
		v.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("%w: unknown key id (keys were fetched recently)", ErrInvalid)
		}
		return err
	}
	v.lastTry = v.now()
	v.mu.Unlock()

	keys, err := v.fetch(ctx)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.fetchErr = err
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoKeys, err)
	}
	v.keys, v.fetched = keys, v.now()
	return nil
}

func (v *Verifier) fetch(ctx context.Context) (map[string]crypto.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var raw []byte
	var err error
	switch {
	case v.cfg.JWKSFile != "":
		raw, err = os.ReadFile(v.cfg.JWKSFile)
	default:
		u := v.cfg.JWKSURL
		if u == "" {
			if u, err = v.discover(ctx); err != nil {
				return nil, err
			}
		}
		raw, err = v.get(ctx, u)
	}
	if err != nil {
		return nil, err
	}
	return ParseJWKS(raw)
}

// discover reads the issuer's OpenID configuration for its JWKS URL.
func (v *Verifier) discover(ctx context.Context) (string, error) {
	v.mu.Lock()
	known := v.jwksURL
	v.mu.Unlock()
	if known != "" {
		return known, nil
	}
	raw, err := v.get(ctx, strings.TrimSuffix(v.cfg.Issuer, "/")+"/.well-known/openid-configuration")
	if err != nil {
		return "", fmt.Errorf("OIDC discovery: %w", err)
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("OIDC discovery: %w", err)
	}
	if doc.Issuer != v.cfg.Issuer {
		return "", fmt.Errorf("OIDC discovery: document is for issuer %q, configured %q", doc.Issuer, v.cfg.Issuer)
	}
	if doc.JWKSURI == "" {
		return "", errors.New("OIDC discovery: no jwks_uri")
	}
	v.mu.Lock()
	v.jwksURL = doc.JWKSURI
	v.mu.Unlock()
	return doc.JWKSURI, nil
}

func (v *Verifier) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ParseJWKS reads a JSON Web Key Set, keeping the signature keys it can use.
func ParseJWKS(raw []byte) (map[string]crypto.PublicKey, error) {
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Crv string `json:"crv"`
			N   string `json:"n"`
			E   string `json:"e"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("JWKS: %w", err)
	}
	out := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		var pub crypto.PublicKey
		switch k.Kty {
		case "RSA":
			n, err1 := b64int(k.N)
			e, err2 := b64int(k.E)
			if err1 != nil || err2 != nil || !e.IsInt64() || n.BitLen() < 2048 {
				continue // malformed, or too weak to trust
			}
			pub = &rsa.PublicKey{N: n, E: int(e.Int64())}
		case "EC":
			var curve elliptic.Curve
			switch k.Crv {
			case "P-256":
				curve = elliptic.P256()
			case "P-384":
				curve = elliptic.P384()
			case "P-521":
				curve = elliptic.P521()
			default:
				continue
			}
			x, err1 := b64int(k.X)
			y, err2 := b64int(k.Y)
			if err1 != nil || err2 != nil {
				continue
			}
			ek := &ecdsa.PublicKey{Curve: curve, X: x, Y: y}
			if _, err := ek.ECDH(); err != nil { // not on the curve
				continue
			}
			pub = ek
		case "OKP":
			x, err := base64.RawURLEncoding.DecodeString(k.X)
			if k.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize {
				continue
			}
			pub = ed25519.PublicKey(x)
		default:
			continue
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, errors.New("JWKS has no usable signing keys")
	}
	return out, nil
}

func b64int(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("bad integer")
	}
	return new(big.Int).SetBytes(b), nil
}

// verifySig checks a JWS signature with the algorithm the header names,
// which must match the key's type.
func verifySig(alg string, key crypto.PublicKey, signed, sig []byte) error {
	digest := func(h hash.Hash) []byte { h.Write(signed); return h.Sum(nil) }
	var hf crypto.Hash
	var h hash.Hash
	switch alg[min(len(alg), 2):] {
	case "256":
		hf, h = crypto.SHA256, sha256.New()
	case "384":
		hf, h = crypto.SHA384, sha512.New384()
	case "512":
		hf, h = crypto.SHA512, sha512.New()
	}
	switch k := key.(type) {
	case *rsa.PublicKey:
		switch {
		case h == nil:
		case strings.HasPrefix(alg, "RS"):
			return rsa.VerifyPKCS1v15(k, hf, digest(h), sig)
		case strings.HasPrefix(alg, "PS"):
			return rsa.VerifyPSS(k, hf, digest(h), sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		}
	case *ecdsa.PublicKey:
		size := (k.Curve.Params().BitSize + 7) / 8
		want := map[string]string{"P-256": "ES256", "P-384": "ES384", "P-521": "ES512"}[k.Curve.Params().Name]
		if alg != want || len(sig) != 2*size {
			break
		}
		r, s := new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:])
		if ecdsa.Verify(k, digest(h), r, s) {
			return nil
		}
		return errors.New("bad signature")
	case ed25519.PublicKey:
		if alg != "EdDSA" {
			break
		}
		if ed25519.Verify(k, signed, sig) {
			return nil
		}
		return errors.New("bad signature")
	}
	return fmt.Errorf("algorithm %q not allowed for this key", alg)
}

// String returns claim path (dots for nested objects, e.g.
// realm_access.roles) as a string, or "".
func (c Claims) String(path string) string {
	s, _ := c.lookup(path).(string)
	return s
}

// Strings returns claim path as a list: an array of strings, or a string
// of space- or comma-separated values.
func (c Claims) Strings(path string) []string {
	switch v := c.lookup(path).(type) {
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == ',' })
	}
	return nil
}

func (c Claims) lookup(path string) any {
	if v, ok := c[path]; ok { // claim names may contain dots themselves (URLs)
		return v
	}
	var cur any = map[string]any(c)
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}
