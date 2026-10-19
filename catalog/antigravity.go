package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/shhac/lib-agent-harness/internal/rawjson"
)

// readAntigravityCatalog reads the JSON output of `agy --output-format json models`.
// Only model IDs and labels are retained; any error or non-model command output
// is rejected without retaining provider text.
func readAntigravityCatalog(reader io.Reader, writer io.Writer) ([]Model, error) {
	if closer, ok := writer.(io.Closer); ok {
		_ = closer.Close()
	}
	bounded := &boundedReader{r: reader, n: outputLimit}
	data, err := io.ReadAll(bounded)
	if errors.Is(err, errOutputLimit) {
		return nil, responseFailure("output_limit")
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, responseFailure("missing_response")
	}
	var reply struct {
		Status  string          `json:"status"`
		Error   json.RawMessage `json:"error"`
		Command *struct {
			Name string `json:"name"`
			Data *struct {
				Models []struct {
					ID    string `json:"id"`
					Label string `json:"label"`
				} `json:"models"`
			} `json:"data"`
		} `json:"command"`
	}
	if json.Unmarshal(data, &reply) != nil {
		return nil, responseFailure("invalid_response")
	}
	if !rawjson.Absent(reply.Error) || (reply.Status != "" && reply.Status != "SUCCESS") {
		return nil, responseFailure("request_failed")
	}
	if reply.Status != "SUCCESS" || reply.Command == nil || reply.Command.Name != "models" || reply.Command.Data == nil || reply.Command.Data.Models == nil {
		return nil, responseFailure("invalid_catalog")
	}
	catalog := newCatalogBuilder()
	for _, m := range reply.Command.Data.Models {
		if err := catalog.add(Model{ID: m.ID, Name: m.Label}); err != nil {
			return nil, err
		}
	}
	return catalog.models, nil
}
