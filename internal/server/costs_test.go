package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
	"github.com/capable78638974979473297813001-pixel/chips/internal/costs"
)

func TestCostsPerClientInDollars(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4}}`)
	})
	ts, _ := setup(t, func(c *config.Config) {
		c.Auth.Keys = []config.APIKey{{Name: "team-x", Key: "kx"}}
		c.Backends[0].CostPerMillionPromptTokens = 1e6 // $1 a token, to make the sums exact
		c.Backends[0].CostPerMillionCompletionTokens = 2e6
		c.Backends[0].HardwareName = "H100"
	}, f.URL)
	for range 2 {
		if resp := chat(t, ts.URL, `{"model":"llama","messages":[{"role":"user","content":"hi"}]}`, "Authorization", "Bearer kx"); resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	req, _ := http.NewRequest("GET", ts.URL+"/admin/costs?days=1", nil)
	req.Header.Set("Authorization", "Bearer kx")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rep costs.Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Clients) != 1 || rep.Clients[0].Client != "team-x" || rep.Clients[0].Dollars != 36 || rep.Clients[0].ByHardware["H100"] != 36 {
		t.Fatalf("report = %+v, want team-x $36 on H100 (2 x (10x$1 + 4x$2))", rep)
	}

	mresp, _ := http.Get(ts.URL + "/metrics")
	body, _ := io.ReadAll(mresp.Body)
	mresp.Body.Close()
	if !strings.Contains(string(body), `siliconrouter_client_cost_dollars_total{client="team-x",hardware="H100"} 36`) {
		t.Errorf("cost metric missing:\n%s", grepLines(string(body), "cost"))
	}
}

func TestCostsRejectsBadRange(t *testing.T) {
	ts, _ := setup(t, nil, newFake(t, nil).URL)
	for _, q := range []string{"days=0", "days=x", "from=yesterday", "from=2026-10-01&to=bad"} {
		resp, err := http.Get(ts.URL + "/admin/costs?" + q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, resp.StatusCode)
		}
	}
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
