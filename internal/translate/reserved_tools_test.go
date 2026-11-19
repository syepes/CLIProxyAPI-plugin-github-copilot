package translate

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestRestoreReservedToolsUndoesHostIntegerRewrite(t *testing.T) {
	t.Parallel()

	original := []byte(`{"tools":[` +
		`{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"yield_time_ms":{"type":"number"}}}},` +
		`{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"wait_agent","parameters":{"type":"object","properties":{"timeout_ms":{"type":"number"}}}}]}` +
		`]}`)
	// The host normalizes both tools to integer for Codex clients.
	prepared := []byte(`{"model":"gpt-6-luna","tools":[` +
		`{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"yield_time_ms":{"type":"integer"}}}},` +
		`{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"wait_agent","parameters":{"type":"object","properties":{"timeout_ms":{"type":"integer"}}}}]}` +
		`]}`)

	out := RestoreReservedTools(prepared, original)

	if got := gjson.GetBytes(out, "tools.1.tools.0.parameters.properties.timeout_ms.type").String(); got != "number" {
		t.Fatalf("collaboration.wait_agent.timeout_ms type = %q, want number: %s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.0.parameters.properties.yield_time_ms.type").String(); got != "integer" {
		t.Fatalf("non-reserved tool changed: yield_time_ms type = %q, want integer: %s", got, out)
	}
	if got := gjson.GetBytes(out, "model").String(); got != "gpt-6-luna" {
		t.Fatalf("model = %q: %s", got, out)
	}
}

func TestRestoreReservedToolsLeavesBodyWithoutReservedNamespace(t *testing.T) {
	t.Parallel()

	prepared := []byte(`{"tools":[{"type":"namespace","name":"clock","tools":[]}]}`)
	for _, original := range [][]byte{nil, []byte(`not json`), []byte(`{"tools":[{"type":"namespace","name":"clock","tools":[{"x":1}]}]}`)} {
		if out := RestoreReservedTools(prepared, original); string(out) != string(prepared) {
			t.Fatalf("body changed for original %q: %s", original, out)
		}
	}
}
