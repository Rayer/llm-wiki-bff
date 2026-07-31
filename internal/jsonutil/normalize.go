// Package jsonutil provides helpers for JSON-safe value conversion.
package jsonutil

import "fmt"

// MaxNormalizationDepth bounds recursive map/slice walks.
const MaxNormalizationDepth = 64

// Normalize converts values produced by YAML parsers (notably yaml.v2 nested
// maps as map[interface{}]interface{}) into types encoding/json can marshal.
// Non-string map keys and excessive nesting return an error.
func Normalize(value interface{}, depth int) (interface{}, error) {
	if depth > MaxNormalizationDepth {
		return nil, fmt.Errorf("maximum nesting depth %d exceeded", MaxNormalizationDepth)
	}

	switch value := value.(type) {
	case map[interface{}]interface{}:
		result := make(map[string]interface{}, len(value))
		for key, item := range value {
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("non-string map key type %T", key)
			}
			normalized, err := Normalize(item, depth+1)
			if err != nil {
				return nil, err
			}
			result[name] = normalized
		}
		return result, nil
	case map[string]interface{}:
		result := make(map[string]interface{}, len(value))
		for key, item := range value {
			normalized, err := Normalize(item, depth+1)
			if err != nil {
				return nil, err
			}
			result[key] = normalized
		}
		return result, nil
	case []interface{}:
		result := make([]interface{}, len(value))
		for i, item := range value {
			normalized, err := Normalize(item, depth+1)
			if err != nil {
				return nil, err
			}
			result[i] = normalized
		}
		return result, nil
	default:
		return value, nil
	}
}

// NormalizeMap normalizes a string-keyed map for JSON encoding.
func NormalizeMap(m map[string]interface{}) (map[string]interface{}, error) {
	if m == nil {
		return map[string]interface{}{}, nil
	}
	normalized, err := Normalize(m, 0)
	if err != nil {
		return nil, err
	}
	out, ok := normalized.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("normalized value type %T is not map[string]interface{}", normalized)
	}
	return out, nil
}
