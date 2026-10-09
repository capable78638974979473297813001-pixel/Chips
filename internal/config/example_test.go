package config

import "testing"

// The shipped examples must always load.
func TestExamplesLoad(t *testing.T) {
	t.Setenv("H100_API_KEY", "x")
	for _, path := range []string{"../../examples/siliconrouter.yaml", "../../examples/demo.yaml", "../../deploy/demo-compose.yaml"} {
		if _, err := Load(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}
