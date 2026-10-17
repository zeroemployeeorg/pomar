package healthreadlimits

// Resources checks the selected explicit-set grammar. It does not register
// identities or infer an authoritative assignment from a reader's own input.
func Resources(ids []string) error {
	if len(ids) == 0 {
		return ErrEmptySet
	}
	if len(ids) > MaxResources {
		return ErrResources
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if len(id) < 1 || len(id) > MaxResourceBytes || seen[id] {
			return ErrResources
		}
		for i := range id {
			c := id[i]
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				return ErrResources
			}
		}
		seen[id] = true
	}
	return nil
}

// SameResources requires two individually valid, nonempty sets. Their order
// is irrelevant; additions, omissions and duplicates refuse the whole read.
// The authorised set must come from independently verified owner state, not
// from unverified claims or a request. This function authenticates neither.
func SameResources(authorised, requested []string) error {
	if err := Resources(authorised); err != nil {
		return err
	}
	if err := Resources(requested); err != nil {
		return err
	}
	if len(authorised) != len(requested) {
		return ErrSetMismatch
	}
	set := make(map[string]bool, len(authorised))
	for _, id := range authorised {
		set[id] = true
	}
	for _, id := range requested {
		if !set[id] {
			return ErrSetMismatch
		}
	}
	return nil
}
