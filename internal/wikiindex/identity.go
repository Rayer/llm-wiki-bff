package wikiindex

// ValidSyntoEntityID accepts only the released Synto entity ULID contract.
func ValidSyntoEntityID(value string) bool {
	if len(value) != 26 || value[0] < '0' || value[0] > '7' {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'H' || r >= 'J' && r <= 'K' || r >= 'M' && r <= 'N' || r >= 'P' && r <= 'T' || r >= 'V' && r <= 'Z') {
			return false
		}
	}
	return true
}

// ValidLegacyConceptID accepts only the content-derived IDs eligible for
// same-slug migration to a Synto entity.
func ValidLegacyConceptID(value string) bool {
	if len(value) != 12 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
