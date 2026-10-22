// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type resolveKeysError string

func (e resolveKeysError) Error() string { return string(e) }

// resolveKeys collects item keys from positional args, or from --keys-from (a
// file or '-' for stdin; lines or JSON). Every key must have Zotero's key
// shape; the first bad one is a usage error (exit 2) naming it, and for a line
// file the line it sits on.
func resolveKeys(args []string, keysFrom string, stdin io.Reader) ([]string, error) {
	if len(args) > 0 && keysFrom != "" {
		return nil, usageErr(resolveKeysError("--keys-from cannot be combined with positional keys"))
	}

	var keys []string
	var badKey error
	where := ""
	seen := make(map[string]struct{})
	addKey := func(key string) {
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		if badKey == nil {
			if err := validateKeyArg("item", key); err != nil {
				badKey = usageErr(fmt.Errorf("%s%w", where, err))
			}
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	if keysFrom == "" {
		for _, arg := range args {
			addKey(arg)
		}
		if badKey != nil {
			return nil, badKey
		}
		if len(keys) == 0 {
			return nil, usageErr(resolveKeysError("no item keys provided"))
		}
		return keys, nil
	}

	data, err := readKeysInput(keysFrom, stdin)
	if err != nil {
		return nil, usageErr(err)
	}

	if keysInputIsJSON(data) {
		if err := addJSONKeys(data, addKey); err != nil {
			return nil, usageErr(err)
		}
	} else if err := addLineKeys(data, addKey, func(n int) { where = fmt.Sprintf("--keys-from line %d: ", n) }); err != nil {
		return nil, usageErr(err)
	}
	if badKey != nil {
		return nil, badKey
	}

	if len(keys) == 0 {
		return nil, usageErr(resolveKeysError("no item keys provided"))
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
	return strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{")
}

func addLineKeys(data []byte, addKey func(string), atLine func(int)) error {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	line := 0
	for scanner.Scan() {
		line++
		atLine(line)
		addKey(strings.TrimSpace(scanner.Text()))
	}
	return scanner.Err()
}

func addJSONKeys(data []byte, addKey func(string)) error {
	trimmed := strings.TrimLeft(string(data), " \t\r\n")
	if strings.HasPrefix(trimmed, "{") {
		return addJSONObjectKeys(data, addKey)
	}

	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return resolveKeysError("malformed keys JSON: " + err.Error())
	}
	for _, entry := range entries {
		if err := addJSONKeyEntry(entry, addKey, false); err != nil {
			return err
		}
	}
	return nil
}

func addJSONObjectKeys(data []byte, addKey func(string)) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return resolveKeysError("malformed keys JSON: " + err.Error())
	}
	if raw, ok := obj["findings"]; ok {
		return addJSONArrayKeys(raw, addKey, true)
	}
	if raw, ok := obj["items"]; ok {
		return addJSONArrayKeys(raw, addKey, false)
	}
	return resolveKeysError("malformed keys JSON: object must contain a findings or items array")
}

func addJSONArrayKeys(data []byte, addKey func(string), allowMissing bool) error {
	if strings.TrimSpace(string(data)) == "null" {
		return nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return resolveKeysError("malformed keys JSON: " + err.Error())
	}
	for _, entry := range entries {
		if err := addJSONKeyEntry(entry, addKey, allowMissing); err != nil {
			return err
		}
	}
	return nil
}

func addJSONKeyEntry(entry json.RawMessage, addKey func(string), allowMissing bool) error {
	trimmed := strings.TrimLeft(string(entry), " \t\r\n")
	if strings.HasPrefix(trimmed, "\"") {
		var key string
		if err := json.Unmarshal(entry, &key); err != nil {
			return resolveKeysError("malformed keys JSON: " + err.Error())
		}
		addKey(strings.TrimSpace(key))
		return nil
	}
	if strings.HasPrefix(trimmed, "{") {
		var obj struct {
			ItemKey *string `json:"item_key"`
			Key     *string `json:"key"`
		}
		if err := json.Unmarshal(entry, &obj); err != nil {
			return resolveKeysError("malformed keys JSON: " + err.Error())
		}
		if obj.ItemKey != nil && strings.TrimSpace(*obj.ItemKey) != "" {
			addKey(strings.TrimSpace(*obj.ItemKey))
			return nil
		}
		if obj.Key != nil && strings.TrimSpace(*obj.Key) != "" {
			addKey(strings.TrimSpace(*obj.Key))
			return nil
		}
		if allowMissing {
			return nil
		}
		return resolveKeysError("malformed keys JSON: entries must be strings or objects with a key or item_key field")
	}
	return resolveKeysError("malformed keys JSON: entries must be strings or objects with a key or item_key field")
}

// resolveTrashKeys is resolveKeys for commands whose Use string already names
// the positionals as <itemKey>...: the root Args wrapper (installPositionalArgUsage)
// has validated their shape before RunE, so they are taken as given and only
// de-duplicated. --keys-from input, the mix with positionals and the empty list
// still go through resolveKeys.
func resolveTrashKeys(args []string, keysFrom string, stdin io.Reader) ([]string, error) {
	if keysFrom != "" || len(args) == 0 {
		return resolveKeys(args, keysFrom, stdin)
	}
	seen := make(map[string]struct{}, len(args))
	keys := make([]string, 0, len(args))
	for _, arg := range args {
		if _, ok := seen[arg]; ok || arg == "" {
			continue
		}
		seen[arg] = struct{}{}
		keys = append(keys, arg)
	}
	if len(keys) == 0 {
		return nil, usageErr(resolveKeysError("no item keys provided"))
	}
	return keys, nil
}
