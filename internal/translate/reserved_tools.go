package translate

import (
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// reservedToolNamespaces lists Codex tool namespaces whose schemas OpenAI models
// validate byte-for-byte ("Function 'collaboration.wait_agent' is reserved for
// use by this model and must match the configured schema").
var reservedToolNamespaces = map[string]struct{}{
	"collaboration": {},
}

// RestoreReservedTools copies reserved namespace tools from the client's
// original Responses request over the host-prepared body. The host rewrites
// some Codex tool parameters from number to integer for non-Codex executors,
// which OpenAI models served by Copilot reject for reserved namespaces.
func RestoreReservedTools(body, original []byte) []byte {
	if len(original) == 0 || !gjson.ValidBytes(original) {
		return body
	}
	originals := make(map[string]string)
	gjson.GetBytes(original, "tools").ForEach(func(_, tool gjson.Result) bool {
		if name, ok := reservedNamespaceName(tool); ok {
			originals[name] = tool.Raw
		}
		return true
	})
	if len(originals) == 0 {
		return body
	}
	out := body
	index := -1
	gjson.GetBytes(body, "tools").ForEach(func(_, tool gjson.Result) bool {
		index++
		name, ok := reservedNamespaceName(tool)
		if !ok {
			return true
		}
		raw, found := originals[name]
		if !found || raw == tool.Raw {
			return true
		}
		if updated, errSet := sjson.SetRawBytes(out, "tools."+strconv.Itoa(index), []byte(raw)); errSet == nil {
			out = updated
		}
		return true
	})
	return out
}

func reservedNamespaceName(tool gjson.Result) (string, bool) {
	if tool.Get("type").String() != "namespace" {
		return "", false
	}
	name := tool.Get("name").String()
	_, ok := reservedToolNamespaces[name]
	return name, ok
}
