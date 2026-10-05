package provider

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Keep provider-native catalog IDs separate from host-facing names.
func (c Config) exposedModelID(upstreamID string) string { return c.ModelPrefix + "/" + upstreamID }

func (c Config) modelAliases(upstreamID string) []string {
	if len(c.Models) == 0 {
		if c.AllowRawModelNames {
			return []string{c.exposedModelID(upstreamID), upstreamID}
		}
		return []string{c.exposedModelID(upstreamID)}
	}
	var aliases []string
	for _, model := range c.Models {
		if model.Name == upstreamID {
			aliases = append(aliases, model.Alias)
		}
	}
	return aliases
}

func (c Config) upstreamModelID(exposedID string) (string, error) {
	if len(c.Models) > 0 {
		for _, model := range c.Models {
			if model.Alias == exposedID {
				return model.Name, nil
			}
		}
		return "", statusError("model_not_found", "model is not in the configured allowlist", 404)
	}
	id, ok := strings.CutPrefix(exposedID, c.ModelPrefix+"/")
	if ok && id != "" {
		return id, nil
	}
	if c.AllowRawModelNames && exposedID != "" {
		return exposedID, nil
	}
	return "", statusError("model_not_found", "Copilot models must use the configured "+c.ModelPrefix+"/ namespace", 404)
}

// Only protocol model metadata is rewritten. Message text, function arguments,
// tool results, and other user-controlled JSON remain untouched.
func rewriteResponseModel(payload []byte, model string) ([]byte, error) {
	for _, path := range []string{"model", "message.model", "response.model"} {
		value := gjson.GetBytes(payload, path)
		if value.Type != gjson.String || value.String() == model {
			continue
		}
		var err error
		payload, err = sjson.SetBytes(payload, path, model)
		if err != nil {
			return nil, err
		}
	}
	return payload, nil
}

// Rewrite complete SSE frames without changing event/id/retry/comment lines.
// A multiline JSON data field is compacted to one valid data line when changed.
func rewriteStreamModel(frame []byte, model string) ([]byte, error) {
	lines := bytes.SplitAfter(frame, []byte("\n"))
	var data [][]byte
	first := -1
	for i, line := range lines {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if first < 0 {
			first = i
		}
		value := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		value = bytes.TrimPrefix(value, []byte("data:"))
		value = bytes.TrimPrefix(value, []byte(" "))
		data = append(data, value)
	}
	payload := bytes.Join(data, []byte("\n"))
	if first < 0 || !json.Valid(payload) {
		return frame, nil
	} // Includes [DONE] and keepalives.
	updated, err := rewriteResponseModel(payload, model)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(updated, payload) {
		return frame, nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, updated); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for i, line := range lines {
		if i == first {
			out.WriteString("data: ")
			out.Write(compact.Bytes())
			if bytes.HasSuffix(line, []byte("\r\n")) {
				out.WriteString("\r\n")
			} else if bytes.HasSuffix(line, []byte("\n")) {
				out.WriteByte('\n')
			}
		} else if !bytes.HasPrefix(line, []byte("data:")) {
			out.Write(line)
		}
	}
	return out.Bytes(), nil
}
