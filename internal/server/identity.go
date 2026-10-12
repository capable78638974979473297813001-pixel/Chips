package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"math"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/audit"
	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/limits"
	"github.com/capable78638974979473297813001-pixel/chips/internal/oidc"
)

// Who is calling: an API key (named or not) or a single sign-on user whose
// JWT the configured OIDC issuer signed.

// keyID is a short fingerprint of an API key for logs: it tells keys apart
// without revealing them.
func keyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "key:" + hex.EncodeToString(sum[:4])
}

// userName is the usage and limits name of a single sign-on user.
func userName(user string) string { return "user:" + user }

// authFailure is why a request was refused at the door.
type authFailure struct {
	status  int
	code    string
	message string
}

// authenticate identifies r's caller. ok is false when auth is on and the
// caller presented nothing valid.
func (s *Server) authenticate(r *http.Request, st *state) (clientInfo, *authFailure) {
	got := clientKey(r.Header)
	if st.verifier != nil && oidc.LooksLikeJWT(got) {
		return s.authenticateJWT(r.Context(), st, got)
	}
	if got != "" {
		// Compare against every key in constant time, so timing does not
		// reveal how much of a guess was right.
		var match clientInfo
		found := false
		for k, c := range st.keys {
			if subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1 {
				match, found = c, true
			}
		}
		if found {
			return match, nil
		}
	}
	return clientInfo{}, &authFailure{http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key or token"}
}

func (s *Server) authenticateJWT(ctx context.Context, st *state, token string) (clientInfo, *authFailure) {
	o := st.cfg.Auth.OIDC
	claims, err := st.verifier.Verify(ctx, token)
	switch {
	case errors.Is(err, oidc.ErrExpired):
		return clientInfo{}, &authFailure{http.StatusUnauthorized, "token_expired", "the access token has expired; get a new one"}
	case errors.Is(err, oidc.ErrNoKeys):
		s.log.Error("cannot verify tokens: identity provider keys unavailable", "issuer", o.Issuer, "error", err)
		return clientInfo{}, &authFailure{http.StatusServiceUnavailable, "auth_unavailable", "cannot verify tokens right now; try again shortly"}
	case err != nil:
		return clientInfo{}, &authFailure{http.StatusUnauthorized, "invalid_token", "invalid access token: " + err.Error()}
	}
	user := claims.String(o.UserClaim)
	if user == "" {
		return clientInfo{}, &authFailure{http.StatusUnauthorized, "invalid_token", "the access token has no " + o.UserClaim + " claim"}
	}
	groups := claims.Strings(o.GroupsClaim)
	c := clientInfo{user: user, name: o.DefaultTeam, priority: 1}
	team := slices.IndexFunc(o.Teams, func(t config.Team) bool { return slices.Contains(groups, t.Group) })
	switch {
	case team >= 0:
		t := o.Teams[team]
		c.name, c.models, c.maxTokens = t.Name, t.Models, t.MaxTokens
		c.priority, _ = config.PriorityRank(t.Priority)
	case o.RequireTeam:
		return clientInfo{}, &authFailure{http.StatusForbidden, "no_team", "your account is in none of the groups allowed to use this service"}
	}
	c.admin = slices.ContainsFunc(groups, func(g string) bool { return slices.Contains(o.AdminGroups, g) })
	return c, nil
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := s.state.Load()
		if !st.cfg.Auth.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		c, fail := s.authenticate(r, st)
		if fail != nil {
			s.refuse(w, r, fail)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKeyCtx{}, c)))
	})
}

func (s *Server) refuse(w http.ResponseWriter, r *http.Request, f *authFailure) {
	s.rejected.Inc("unauthorized")
	s.audit.Log("auth_failure", map[string]any{"reason": f.code, "method": r.Method, "path": r.URL.Path,
		"remote_addr": remoteIP(r), "forwarded_for": r.Header.Get("X-Forwarded-For")})
	writeError(w, f.status, f.code, f.message)
}

// requireAdmin guards /admin/*: admin keys and members of
// auth.oidc.admin_groups when either is configured, otherwise any client
// (or anyone, when auth is off). Every call is audited.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := s.state.Load()
		actor := "anonymous"
		restricted := len(st.adminKeys) > 0 || len(st.cfg.Auth.OIDC.AdminGroups) > 0
		got := clientKey(r.Header)
		switch {
		case restricted && got != "" && slices.ContainsFunc(st.adminKeys, func(k string) bool {
			return subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1
		}):
			actor = keyID(got)
		case restricted:
			c, fail := s.authenticate(r, st)
			if fail == nil && !c.admin {
				fail = &authFailure{http.StatusUnauthorized, "invalid_admin_key", "this endpoint needs an admin key or an admin group"}
			}
			if fail != nil {
				if fail.code == "invalid_api_key" {
					fail = &authFailure{http.StatusUnauthorized, "invalid_admin_key", "this endpoint needs an admin key"}
				}
				s.refuse(w, r, fail)
				return
			}
			actor = userName(c.user)
		case st.cfg.Auth.Enabled():
			c, fail := s.authenticate(r, st)
			if fail != nil {
				s.refuse(w, r, fail)
				return
			}
			actor = c.actor()
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.audit.Log("admin", map[string]any{"actor": actor, "method": r.Method, "path": r.URL.Path,
			"status": rec.status, "remote_addr": remoteIP(r)})
	})
}

// actor names the client in audit entries.
func (c clientInfo) actor() string {
	switch {
	case c.user != "":
		return userName(c.user)
	case c.key != "":
		return keyID(c.key)
	}
	return "anonymous"
}

// statusRecorder remembers the status a handler sent.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func remoteIP(r *http.Request) string {
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return ip
	}
	return r.RemoteAddr
}

// admit checks the limits of the client's team (or key) and, for single
// sign-on users, the user's own. It returns one release for both.
func (s *Server) admit(who string, c clientInfo) (func(), *limits.Rejection) {
	release, rejected := s.limits.Admit(who)
	if rejected != nil || c.user == "" {
		return release, rejected
	}
	releaseUser, rejected := s.limits.Admit(userName(c.user))
	if rejected != nil {
		release()
		rejected.Message = strings.Replace(rejected.Message, "this API key", "your account", 1)
		return nil, rejected
	}
	return func() { releaseUser(); release() }, nil
}

// userDefaults are the limits every single sign-on user gets.
func userDefaults(cfg *config.Config) map[string]limits.Limits {
	u := cfg.Auth.OIDC.UserLimits
	return map[string]limits.Limits{"user:": {MaxConcurrent: u.MaxConcurrent, RequestsPerMinute: u.RequestsPerMinute, TokensPerDay: u.TokensPerDay}}
}

// newVerifier keeps prev's verifier (and its fetched keys) when the OIDC
// settings did not change.
func newVerifier(cfg *config.Config, prev *state, client *http.Client) *oidc.Verifier {
	o := cfg.Auth.OIDC
	if !o.Enabled() {
		return nil
	}
	if prev != nil && prev.verifier != nil && sameOIDC(prev.cfg.Auth.OIDC, o) {
		return prev.verifier
	}
	return oidc.New(oidc.Config{Issuer: o.Issuer, Audiences: o.Audiences, JWKSURL: o.JWKSURL, JWKSFile: o.JWKSFile,
		Leeway: o.Leeway.D(), Refresh: o.JWKSRefresh.D()}, client)
}

func sameOIDC(a, b config.OIDC) bool {
	return a.Issuer == b.Issuer && slices.Equal(a.Audiences, b.Audiences) && a.JWKSURL == b.JWKSURL &&
		a.JWKSFile == b.JWKSFile && a.Leeway == b.Leeway && a.JWKSRefresh == b.JWKSRefresh
}

// RefreshIdentityKeys fetches the identity provider's signing keys, so a
// wrong issuer shows up at startup rather than on the first request.
func (s *Server) RefreshIdentityKeys(ctx context.Context) error {
	if v := s.state.Load().verifier; v != nil {
		return v.Refresh(ctx)
	}
	return nil
}

// SetAudit sends audit entries to l (nil turns them off).
func (s *Server) SetAudit(l *audit.Logger) { s.audit = l }

// auditRequest writes one inference request to the audit log: who, what
// model, where it ran, the outcome, tokens and cost. Never content.
func (s *Server) auditRequest(l *requestLog, backend string, status, attempts int, elapsed time.Duration, err error) {
	if s.audit == nil || !s.conf().Audit.LogRequests() {
		return
	}
	e := map[string]any{
		"client": l.client.name, "actor": l.client.actor(), "method": l.r.Method, "path": l.r.URL.Path,
		"model": l.info.Model, "backend": backend, "status": status, "attempts": attempts,
		"duration_ms": elapsed.Milliseconds(), "remote_addr": remoteIP(l.r), "trace_id": l.trace.TraceID.String(),
	}
	if l.promptTokens > 0 || l.completionTokens > 0 {
		e["prompt_tokens"], e["completion_tokens"] = l.promptTokens, l.completionTokens
	}
	if l.cost > 0 {
		e["cost_usd"] = math.Round(l.cost*1e6) / 1e6
	}
	if l.fallback != "" {
		e["fallback"] = l.fallback
	}
	if l.alias != "" {
		e["alias"] = l.alias
	}
	if l.cache != "" {
		e["response_cache"] = l.cache
	}
	if l.kind != "" {
		e["kind"] = l.kind
	}
	if err != nil {
		e["error"] = err.Error()
	}
	s.audit.Log("request", e)
}
