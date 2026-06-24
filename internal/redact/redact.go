package redact

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/config"
)

// redactedB64 is "REDACTED" base64-encoded.  Kubernetes Secret data values are
// base64 strings, so we replace with another valid base64 string.
var redactedB64 = base64.StdEncoding.EncodeToString([]byte("REDACTED"))

// Options controls what Archive() redacts.
type Options struct {
	// RedactSecrets, when true, replaces all Kubernetes Secret data and
	// stringData values with "REDACTED".
	RedactSecrets bool
	// AllowList is a set of "namespace/name" secret keys whose data is preserved
	// even when RedactSecrets is true.
	AllowList map[string]bool
	// Rules is the list of field-level redaction rules to apply to every record.
	Rules []config.RedactionRule
}

// Result reports how many redactions were performed.
type Result struct {
	SecretsRedacted int
	FieldsRedacted  int
}

// Archive reads srcPath, applies redaction options, and writes to dstPath.
// The original archive is not modified.
func Archive(srcPath, dstPath string, opts Options) (Result, error) {
	ar, err := archive.Open(srcPath)
	if err != nil {
		return Result{}, fmt.Errorf("opening archive: %w", err)
	}
	defer ar.Close()

	var meta capture.CaptureMetadata
	if err := ar.ReadMetadata(&meta); err != nil {
		return Result{}, fmt.Errorf("reading metadata: %w", err)
	}
	if err := capture.CheckFormatVersion(meta); err != nil {
		return Result{}, err
	}
	// The redacted archive is written by the current writer, so stamp it with
	// the current format version.
	meta.FormatVersion = capture.CurrentFormatVersion

	var idx capture.Index
	if err := ar.ReadIndex(&idx); err != nil {
		return Result{}, fmt.Errorf("reading index: %w", err)
	}

	var wi capture.WatchIndex
	_, _ = ar.ReadWatchIndex(&wi)

	result := Result{}

	sw, err := archive.NewStreamWriter(dstPath)
	if err != nil {
		return Result{}, fmt.Errorf("creating output archive: %w", err)
	}

	// newIdx / newWI will be built with corrected seq numbers as we write.
	newIdx := make(capture.Index, len(idx))
	newWI := make(capture.WatchIndex, len(wi))

	// Copy regular index records in order, rebuilding seqs.
	for apiPath, entry := range idx {
		newEntry := &capture.IndexEntry{
			APIPath: entry.APIPath,
			Times:   entry.Times,
		}
		for _, seq := range entry.Seqs {
			data, err := ar.ReadRecord(apiPath, seq)
			if err != nil {
				return Result{}, fmt.Errorf("reading record seq %d: %w", seq, err)
			}
			var rec capture.Record
			if err := json.Unmarshal(data, &rec); err != nil {
				return Result{}, fmt.Errorf("parsing record seq %d: %w", seq, err)
			}

			if opts.RedactSecrets {
				secretsRedacted, err := redactRecord(&rec, opts.AllowList)
				if err != nil {
					return Result{}, fmt.Errorf("redacting record seq %d: %w", seq, err)
				}
				if secretsRedacted {
					result.SecretsRedacted++
				}
			}

			if len(opts.Rules) > 0 {
				fieldsRedacted, err := redactFieldsInRecord(&rec, opts.Rules)
				if err != nil {
					return Result{}, fmt.Errorf("field-redacting record seq %d: %w", seq, err)
				}
				result.FieldsRedacted += fieldsRedacted
			}

			newSeq, err := sw.WriteRecordRaw(apiPath, rec)
			if err != nil {
				return Result{}, fmt.Errorf("writing record seq %d: %w", seq, err)
			}
			newEntry.Seqs = append(newEntry.Seqs, newSeq)
		}
		newIdx[apiPath] = newEntry
	}

	// Copy watch-index records in order.
	for apiPath, wiEntry := range wi {
		newWIEntry := &capture.WatchIndexEntry{
			APIPath:    wiEntry.APIPath,
			Times:      wiEntry.Times,
			EventTypes: wiEntry.EventTypes,
		}
		for _, seq := range wiEntry.Seqs {
			data, err := ar.ReadRecord(apiPath, seq)
			if err != nil {
				return Result{}, fmt.Errorf("reading watch record seq %d: %w", seq, err)
			}
			var rec capture.Record
			if err := json.Unmarshal(data, &rec); err != nil {
				return Result{}, fmt.Errorf("parsing watch record seq %d: %w", seq, err)
			}

			if opts.RedactSecrets {
				secretsRedacted, err := redactRecord(&rec, opts.AllowList)
				if err != nil {
					return Result{}, fmt.Errorf("redacting watch record seq %d: %w", seq, err)
				}
				if secretsRedacted {
					result.SecretsRedacted++
				}
			}

			if len(opts.Rules) > 0 {
				fieldsRedacted, err := redactFieldsInRecord(&rec, opts.Rules)
				if err != nil {
					return Result{}, fmt.Errorf("field-redacting watch record seq %d: %w", seq, err)
				}
				result.FieldsRedacted += fieldsRedacted
			}

			newSeq, err := sw.WriteRecordRaw(apiPath, rec)
			if err != nil {
				return Result{}, fmt.Errorf("writing watch record seq %d: %w", seq, err)
			}
			newWIEntry.Seqs = append(newWIEntry.Seqs, newSeq)
		}
		newWI[apiPath] = newWIEntry
	}

	// Record that this archive was redacted so the UI / inspect can surface it.
	meta.Redacted = true
	meta.SecretsRedacted = result.SecretsRedacted
	meta.FieldsRedacted = result.FieldsRedacted

	if err := sw.Finish(&meta, newIdx, newWI); err != nil {
		return Result{}, fmt.Errorf("finishing output archive: %w", err)
	}

	return result, nil
}

// redactFieldsInRecord applies field-level redaction rules to rec in-place.
// Returns the count of fields modified.
func redactFieldsInRecord(rec *capture.Record, rules []config.RedactionRule) (int, error) {
	var obj map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &obj); err != nil {
		// Not a JSON object (e.g. Table format) — skip.
		return 0, nil
	}

	changed, err := ApplyRules(obj, rules)
	if err != nil {
		return 0, err
	}
	if !changed {
		return 0, nil
	}

	newBody, err := json.Marshal(obj)
	if err != nil {
		return 0, fmt.Errorf("re-marshalling record: %w", err)
	}
	rec.ResponseBody = newBody
	return 1, nil
}

// redactRecord modifies rec in-place if it contains Secret data.
// Handles both individual Secret objects ("kind":"Secret") and list responses
// ("kind":"SecretList") since the capture engine stores list-level responses.
// Returns true if any redaction was performed.
func redactRecord(rec *capture.Record, allowList map[string]bool) (bool, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rec.ResponseBody, &obj); err != nil {
		// Not JSON (e.g. Table format) — leave as-is.
		return false, nil
	}

	kindRaw, ok := obj["kind"]
	if !ok {
		return false, nil
	}
	var kind string
	if err := json.Unmarshal(kindRaw, &kind); err != nil {
		return false, nil
	}

	switch kind {
	case "Secret":
		return redactSecretObj(obj, allowList, &rec.ResponseBody)
	case "SecretList":
		return redactSecretList(obj, allowList, &rec.ResponseBody)
	default:
		return false, nil
	}
}

// redactSecretObj redacts data/stringData on a single Secret map.
// Writes back to dest if modified.
func redactSecretObj(obj map[string]json.RawMessage, allowList map[string]bool, dest *json.RawMessage) (bool, error) {
	if allowList[secretKey(obj)] {
		return false, nil
	}

	modified := false

	if dataRaw, ok := obj["data"]; ok {
		var dataMap map[string]string
		if err := json.Unmarshal(dataRaw, &dataMap); err == nil && len(dataMap) > 0 {
			for k := range dataMap {
				dataMap[k] = redactedB64
			}
			newData, _ := json.Marshal(dataMap)
			obj["data"] = newData
			modified = true
		}
	}

	if sdRaw, ok := obj["stringData"]; ok {
		var sdMap map[string]string
		if err := json.Unmarshal(sdRaw, &sdMap); err == nil && len(sdMap) > 0 {
			for k := range sdMap {
				sdMap[k] = "REDACTED"
			}
			newSD, _ := json.Marshal(sdMap)
			obj["stringData"] = newSD
			modified = true
		}
	}

	if modified {
		newBody, err := json.Marshal(obj)
		if err != nil {
			return false, fmt.Errorf("re-marshalling secret: %w", err)
		}
		*dest = newBody
	}
	return modified, nil
}

// redactSecretList redacts all items in a SecretList response.
func redactSecretList(obj map[string]json.RawMessage, allowList map[string]bool, dest *json.RawMessage) (bool, error) {
	itemsRaw, ok := obj["items"]
	if !ok {
		return false, nil
	}

	var items []json.RawMessage
	if err := json.Unmarshal(itemsRaw, &items); err != nil || len(items) == 0 {
		return false, nil
	}

	modified := false
	for i, itemRaw := range items {
		var itemObj map[string]json.RawMessage
		if err := json.Unmarshal(itemRaw, &itemObj); err != nil {
			continue
		}
		var newBody json.RawMessage = itemRaw
		changed, err := redactSecretObj(itemObj, allowList, &newBody)
		if err != nil {
			return false, err
		}
		if changed {
			items[i] = newBody
			modified = true
		}
	}

	if modified {
		newItems, err := json.Marshal(items)
		if err != nil {
			return false, fmt.Errorf("re-marshalling secret list items: %w", err)
		}
		obj["items"] = newItems
		newBody, err := json.Marshal(obj)
		if err != nil {
			return false, fmt.Errorf("re-marshalling secret list: %w", err)
		}
		*dest = newBody
	}
	return modified, nil
}

// secretKey returns "namespace/name" for a Secret object map, for allowlist lookup.
func secretKey(obj map[string]json.RawMessage) string {
	metaRaw, ok := obj["metadata"]
	if !ok {
		return ""
	}
	var meta struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	}
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return ""
	}
	if meta.Namespace == "" {
		return meta.Name
	}
	return strings.Join([]string{meta.Namespace, meta.Name}, "/")
}
