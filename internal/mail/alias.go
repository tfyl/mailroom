package mail

import (
	"fmt"
	"strings"
)

// MaxAliasLen bounds an alias so it stays readable everywhere it is echoed — tool results,
// grant records, audit rows — and so a pathological one cannot be used to bloat them.
const MaxAliasLen = 64

// ParseAlias normalises and checks a user-supplied alias.
//
// The character set is deliberately narrow. An alias is typed into tool calls by an agent and
// read back out of every result, so anything that needs quoting, changes under normalisation,
// or reads like an address is more trouble than the freedom is worth. The linking forms have
// advertised exactly this rule as an HTML pattern since the first version; enforcing it here
// is what makes it true of a crafted POST as well.
func ParseAlias(raw string) (string, error) {
	alias := strings.TrimSpace(raw)
	switch {
	case alias == "":
		return "", fmt.Errorf("an alias is required")
	case len(alias) > MaxAliasLen:
		return "", fmt.Errorf("an alias may be at most %d characters", MaxAliasLen)
	}

	for _, r := range alias {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return "", fmt.Errorf("an alias may only contain letters, numbers, dashes and underscores")
		}
	}
	return alias, nil
}
