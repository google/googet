// Package priority provides standard priority levels.
package priority

import "strconv"

// Value is an integer priority associated with a repo.
type Value int

const (
	// None is an unspecified priority level.
	None Value = 0
	// Default is the default priority level for everything.
	Default Value = 500
	// Canary is the priority level for canary repos.
	Canary Value = 1300
	// Pin is the priority level for user-pinned repos.
	Pin Value = 1400
	// Rollback is the priority level for rollback repos.
	Rollback Value = 1500
)

// priorityNameToValue maps semantic priority names to their integer values.
var priorityNameToValue = map[string]Value{
	"default":  Default,
	"canary":   Canary,
	"pin":      Pin,
	"rollback": Rollback,
}

// priorityValueToName maps integer priorities to their semantic names.
var priorityValueToName = map[Value]string{
	Default:  "default",
	Canary:   "canary",
	Pin:      "pin",
	Rollback: "rollback",
}

// FromString converts the string s into a priority Value, where s represents
// either a semantic priority name or an integer.
func FromString(s string) (Value, error) {
	if v, ok := priorityNameToValue[s]; ok {
		return v, nil
	}
	i, err := strconv.Atoi(s)
	return Value(i), err
}

// MarshalYAML marshals a priority value as a semantic name if possible
// otherwise as an integer.
func (v Value) MarshalYAML() (any, error) {
	if name, ok := priorityValueToName[v]; ok {
		return name, nil
	}
	return int(v), nil
}
