// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean write-safety): add shared item-key selection for write commands.

package cli

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
)

type resolveKeysError string

func (e resolveKeysError) Error() string { return string(e) }

func resolveKeys(args []string, keysFrom string, stdin io.Reader) ([]string, error) {
	if len(args) > 0 && keysFrom != "" {
		return nil, resolveKeysError("--keys-from cannot be combined with positional keys")
	}

	var keys []string
	seen := make(map[string]struct{})
	addKey := func(key string) {
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	if keysFrom == "" {
		for _, arg := range args {
			addKey(arg)
		}
		if len(keys) == 0 {
			return nil, resolveKeysError("no item keys provided")
		}
		return keys, nil
	}

	data, err := readKeysInput(keysFrom, stdin)
	if err != nil {
		return nil, err
	}

	if keysInputIsJSON(data) {
		if err := addJSONKeys(data, addKey); err != nil {
			return nil, err
		}
	} else if err := addLineKeys(data, addKey); err != nil {
		return nil, err
	}

	if len(keys) == 0 {
		return nil, resolveKeysError("no item keys provided")
	}
	return keys, nil
}

func readKeysInput(keysFrom string, stdin io.Reader) ([]byte, error) {
	if keysFrom == "-" {
		if stdin == nil {
			stdin = strings.NewReader("")
		}
		return io.ReadAll(stdin)
	}
	return os.ReadFile(keysFrom)
}

func keysInputIsJSON(data []byte) bool {
	trimmed := strings.TrimLeft(string(data), " \t\r\n")
	return strings.HasPrefix(trimmed, "[")
}

func addLineKeys(data []byte, addKey func(string)) error {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		addKey(strings.TrimSpace(scanner.Text()))
	}
	return scanner.Err()
}

func addJSONKeys(data []byte, addKey func(string)) error {
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return resolveKeysError("malformed keys JSON: " + err.Error())
	}
	for _, entry := range entries {
		trimmed := strings.TrimLeft(string(entry), " \t\r\n")
		if strings.HasPrefix(trimmed, "\"") {
			var key string
			if err := json.Unmarshal(entry, &key); err != nil {
				return resolveKeysError("malformed keys JSON: " + err.Error())
			}
			addKey(strings.TrimSpace(key))
			continue
		}
		if strings.HasPrefix(trimmed, "{") {
			var obj struct {
				Key *string `json:"key"`
			}
			if err := json.Unmarshal(entry, &obj); err != nil || obj.Key == nil {
				return resolveKeysError("malformed keys JSON: entries must be strings or objects with a key field")
			}
			addKey(strings.TrimSpace(*obj.Key))
			continue
		}
		return resolveKeysError("malformed keys JSON: entries must be strings or objects with a key field")
	}
	return nil
}
