package prompt

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"slices"
	"strings"
)

// Claims is everything a request says about which model it wants and how
// many tokens it may generate, gathered for enforcing per-key policy.
// Routing reads requests leniently (see Inspect); policy must not, because
// a backend that reads a field the router missed would bypass it.
type Claims struct {
	// Models are all the model names the request carries: in the body,
	// the URL path and the query string.
	Models []string
	// Budget is the largest completion budget any budget field states, so
	// a cap cannot be dodged by setting two of them; -1 when one asks for
	// no limit or is not a number. BudgetSet reports whether any is present.
	Budget    int
	BudgetSet bool
	// Ambiguous is true when parsers could disagree about the request: a
	// policy field given twice or with escaped characters in its key, or a
	// body that is not well-formed. Such requests should be refused.
	Ambiguous bool
}

// Budget fields by API. Top level: OpenAI chat and completions, Anthropic,
// OpenAI Responses. Nested: TGI (parameters), Ollama (options) and Gemini
// (generationConfig, whose proto JSON also accepts snake_case).
var (
	topBudget     = []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}
	budgetObjects = []string{"parameters", "options", "generationConfig", "generation_config"}
	innerBudget   = []string{"max_new_tokens", "num_predict", "maxOutputTokens", "max_output_tokens", "max_tokens"}
	topFields     = append(append([]string{"model"}, topBudget...), budgetObjects...)
)

// policyKey matches a raw object key against names the way the most
// lenient backend would: after unescaping, and ignoring case (Go's
// encoding/json, used by Ollama and others, folds case). exact is false
// when the key matched only after unescaping or folding, which a strict
// parser would read differently.
func policyKey(raw []byte, names []string) (name string, exact bool) {
	k, escaped := decodeKey(raw)
	for _, n := range names {
		if strings.EqualFold(k, n) {
			return n, !escaped && k == n
		}
	}
	return "", false
}

// ClaimsOf reads a request's claims. query is the raw query string.
func ClaimsOf(path, rawQuery, contentType string, body []byte) Claims {
	var c Claims
	if m := ModelFromPath(path); m != "" {
		c.Models = append(c.Models, m)
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		c.Ambiguous = true
	}
	c.Models = append(c.Models, q["model"]...)

	mediaType, params, _ := mime.ParseMediaType(contentType)
	switch {
	case len(bytes.TrimSpace(body)) == 0:
	case mediaType == "multipart/form-data":
		c.multipart(body, params["boundary"])
	case mediaType == "application/x-www-form-urlencoded":
		form, err := url.ParseQuery(string(body))
		if err != nil {
			c.Ambiguous = true
		}
		if len(form["model"]) > 1 {
			c.Ambiguous = true
		}
		c.Models = append(c.Models, form["model"]...)
	default:
		c.json(body)
	}
	return c
}

func (c *Claims) json(body []byte) {
	end, err := skipValue(body, skipSpace(body, 0))
	if err != nil || skipSpace(body, end) != len(body) {
		c.Ambiguous = true
		return
	}
	seen := map[string]bool{}
	err = eachField(body, func(raw, v []byte) bool {
		k, exact := policyKey(raw, topFields)
		if k == "" {
			return true
		}
		if !exact || seen[k] {
			c.Ambiguous = true
		}
		seen[k] = true
		switch {
		case k == "model":
			if s, ok := stringValue(v); ok {
				c.Models = append(c.Models, s)
			} else if string(v) != "null" {
				c.Ambiguous = true
			}
		case slices.Contains(topBudget, k):
			c.budget(v)
		case string(v) == "null":
		default: // a budget object
			inner := map[string]bool{}
			if eachField(v, func(raw, v []byte) bool {
				k, exact := policyKey(raw, innerBudget)
				if k == "" {
					return true
				}
				if !exact || inner[k] {
					c.Ambiguous = true
				}
				inner[k] = true
				c.budget(v)
				return true
			}) != nil {
				c.Ambiguous = true
			}
		}
		return true
	})
	if err != nil {
		c.Ambiguous = true // not an object, or malformed
	}
}

func (c *Claims) budget(v []byte) {
	c.BudgetSet = true
	n, ok := numberValue(v)
	switch {
	case !ok || n < 0: // null, a string, or Ollama's -1/-2: no cap the router can see
		c.Budget = -1
	case c.Budget >= 0 && n > float64(c.Budget):
		c.Budget = int(min(n, 1<<31-1))
	}
}

func (c *Claims) multipart(body []byte, boundary string) {
	if boundary == "" {
		c.Ambiguous = true
		return
	}
	r := multipart.NewReader(bytes.NewReader(body), boundary)
	n := 0
	for {
		part, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			c.Ambiguous = true
			return
		}
		if part.FormName() == "model" {
			v, _ := io.ReadAll(io.LimitReader(part, 512))
			c.Models = append(c.Models, strings.TrimSpace(string(v)))
			n++
		}
	}
	if n > 1 {
		c.Ambiguous = true
	}
}

// decodeKey unescapes a raw JSON object key; escaped reports whether it
// contained escapes at all.
func decodeKey(raw []byte) (key string, escaped bool) {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw), false
	}
	var s string
	if json.Unmarshal(append(append([]byte{'"'}, raw...), '"'), &s) != nil {
		return string(raw), true
	}
	return s, true
}

// AddField returns body, a JSON object, with a top-level key set to value
// inserted first, leaving every other byte as it was. ok is false if body
// is not an object.
func AddField(body []byte, key string, value any) (out []byte, ok bool) {
	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return body, false
	}
	k, _ := json.Marshal(key)
	v, err := json.Marshal(value)
	if err != nil {
		return body, false
	}
	out = make([]byte, 0, len(body)+len(k)+len(v)+2)
	out = append(out, body[:i+1]...)
	out = append(out, k...)
	out = append(out, ':')
	out = append(out, v...)
	if j := skipSpace(body, i+1); j < len(body) && body[j] != '}' {
		out = append(out, ',')
	}
	return append(out, body[i+1:]...), true
}
