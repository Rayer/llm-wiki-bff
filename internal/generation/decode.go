package generation

import (
	"encoding/json"
	"errors"
	"io"
)

// ErrLogicalEntryLimit is returned when a generated cache contains more
// logical rows than the worker output contract permits.
var ErrLogicalEntryLimit = errors.New("generated cache logical entry limit exceeded")

// DecodeBoundedMap decodes a JSON object without growing its result beyond
// the shared generated-output logical entry budget.
func DecodeBoundedMap[T any](dec *json.Decoder) (map[string]T, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("expected JSON object")
	}
	result := make(map[string]T)
	entries := 0
	for dec.More() {
		if entries >= MaxFiles {
			return nil, ErrLogicalEntryLimit
		}
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("expected JSON object key")
		}
		var value T
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		result[name] = value
		entries++
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return result, nil
}

// DecodeBoundedStrings decodes a JSON array of strings under the shared
// generated-output logical entry budget.
func DecodeBoundedStrings(dec *json.Decoder) ([]string, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '[' {
		return nil, errors.New("expected JSON array")
	}
	values := make([]string, 0)
	for dec.More() {
		if len(values) >= MaxFiles {
			return nil, ErrLogicalEntryLimit
		}
		var value string
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return values, nil
}

// DecodeBoundedStringLists decodes an object whose values are string arrays.
// The number of keys and each nested list are bounded while streaming, before
// either collection can grow past MaxFiles.
func DecodeBoundedStringLists(dec *json.Decoder) (map[string][]string, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("expected JSON object")
	}
	result := make(map[string][]string)
	keys := 0
	for dec.More() {
		if keys >= MaxFiles {
			return nil, ErrLogicalEntryLimit
		}
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("expected JSON object key")
		}
		list, err := DecodeBoundedStrings(dec)
		if err != nil {
			return nil, err
		}
		result[name] = list
		keys++
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return result, nil
}

// EnsureJSONEOF rejects trailing JSON values after a bounded stream decode.
func EnsureJSONEOF(dec *json.Decoder) error {
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON")
		}
		return err
	}
	return nil
}
