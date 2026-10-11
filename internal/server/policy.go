package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/prompt"
)

// restricted reports whether the client's key limits its models or tokens.
func (c clientInfo) restricted() bool { return len(c.models) > 0 || c.maxTokens > 0 }

// allows reports whether the client may use model, named directly or
// through an alias.
func (c clientInfo) allows(aliases map[string]string, model string) bool {
	if len(c.models) == 0 {
		return true
	}
	if config.MatchModel(c.models, model) {
		return true
	}
	target, ok := aliases[model]
	return ok && config.MatchModel(c.models, target)
}

// enforcePolicy applies a key's model allow-list and completion-token cap.
// It returns the body to forward, with the cap stated when the request
// names no budget and its API has a field for one, or answers the request
// itself and returns false.
func (s *Server) enforcePolicy(w http.ResponseWriter, r *http.Request, st *state, c clientInfo, body []byte) ([]byte, bool) {
	claims := prompt.ClaimsOf(r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type"), body)
	if claims.Ambiguous {
		s.rejected.Inc("policy")
		writeError(w, http.StatusBadRequest, "invalid_request",
			"this API key's model and token policy needs a well-formed request with each of model and token-limit fields given once, in plain lowercase")
		return nil, false
	}
	if !s.modelsAllowed(w, st, c, claims.Models) {
		return nil, false
	}
	if c.maxTokens > 0 {
		if claims.Budget < 0 || claims.Budget > c.maxTokens {
			s.rejected.Inc("policy")
			writeError(w, http.StatusBadRequest, "max_tokens_exceeded",
				"this API key allows at most "+strconv.Itoa(c.maxTokens)+" completion tokens per request")
			return nil, false
		}
		if field := budgetField(r.URL.Path); !claims.BudgetSet && field != "" {
			if out, ok := prompt.AddField(body, field, c.maxTokens); ok {
				body = out
			}
		}
	}
	return body, true
}

// modelsAllowed checks every model a request names against the client's
// allow-list, answering the request itself and returning false if one is
// not allowed or none is named.
func (s *Server) modelsAllowed(w http.ResponseWriter, st *state, c clientInfo, models []string) bool {
	if len(c.models) == 0 {
		return true
	}
	if len(models) == 0 {
		s.rejected.Inc("policy")
		writeError(w, http.StatusForbidden, "model_not_allowed", "this API key must name a model it is allowed to use")
		return false
	}
	for _, m := range models {
		if !c.allows(st.cfg.Models.Aliases, m) {
			s.rejected.Inc("policy")
			writeError(w, http.StatusForbidden, "model_not_allowed", "this API key may not use model "+strconv.Quote(m))
			return false
		}
	}
	return true
}

// budgetField is the completion-budget field of the API at path, for APIs
// where a request without one may otherwise run to the context limit.
func budgetField(path string) string {
	switch {
	case strings.HasSuffix(path, "/responses"): // OpenAI Responses
		return "max_output_tokens"
	case strings.HasSuffix(path, "/completions"), strings.HasSuffix(path, "/messages"): // OpenAI, Anthropic
		return "max_tokens"
	}
	return ""
}
