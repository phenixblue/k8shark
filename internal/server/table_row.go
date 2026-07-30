package server

import (
	"encoding/json"
	"fmt"
)

// extractTableRow finds the row matching name in a Table response and returns
// a single-row Table with the same columnDefinitions. This is used when kubectl
// requests a single object by name — the per-name ?as=Table isn't captured, but
// the parent list's Table is, and we can slice the right row out of it.
func extractTableRow(tableBody []byte, name string) ([]byte, error) {
	var table struct {
		APIVersion        string            `json:"apiVersion"`
		Kind              string            `json:"kind"`
		Metadata          json.RawMessage   `json:"metadata"`
		ColumnDefinitions json.RawMessage   `json:"columnDefinitions"`
		Rows              []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(tableBody, &table); err != nil {
		return nil, err
	}
	for _, row := range table.Rows {
		var r struct {
			Object struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"object"`
		}
		if err := json.Unmarshal(row, &r); err != nil {
			continue
		}
		if r.Object.Metadata.Name == name {
			return json.Marshal(map[string]any{
				"apiVersion":        table.APIVersion,
				"kind":              table.Kind,
				"metadata":          table.Metadata,
				"columnDefinitions": table.ColumnDefinitions,
				"rows":              []json.RawMessage{row},
			})
		}
	}
	return nil, fmt.Errorf("row %q not found in table", name)
}
