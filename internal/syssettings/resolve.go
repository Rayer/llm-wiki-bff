package syssettings

import "strings"

// Resolve picks registration_enabled using this order:
//  1. If the Firestore system/settings document exists, use its registration_enabled value.
//  2. Else if REGISTRATION_ENABLED env is set, use that parsed value.
//  3. Else default true for backward compatibility in current dev (open signup until
//     an admin disables it or prod sets REGISTRATION_ENABLED=false).
func Resolve(docExists bool, firestoreValue bool, envValue *bool) bool {
	if docExists {
		return firestoreValue
	}
	if envValue != nil {
		return *envValue
	}
	return true
}

// ParseEnvBool parses REGISTRATION_ENABLED env values: true/false/1/0.
func ParseEnvBool(raw string) (bool, bool) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	switch raw {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		return false, false
	}
}