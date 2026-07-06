// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH: Share JSON field extraction for hand-written item workflow commands.

package cli

import (
	"encoding/json"
	"fmt"
)

func jsonStringField(raw json.RawMessage, field string) string {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	return jsonStringFieldFromMap(obj, field)
}

func jsonStringFieldFromMap(obj map[string]any, field string) string {
	if value, ok := stringValue(obj[field]); ok {
		return value
	}
	dataObj, ok := obj["data"].(map[string]any)
	if !ok {
		return ""
	}
	if value, ok := stringValue(dataObj[field]); ok {
		return value
	}
	return ""
}

func stringValue(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case fmt.Stringer:
		return v.String(), true
	default:
		return "", false
	}
}
