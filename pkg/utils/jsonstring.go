package utils

import "encoding/json"

// jsonString returns s as a quoted JSON string.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
