// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Hand-written `items file` (not in the generated CLI). Resolves the on-disk
// location of an item's attachment via the Zotero local API's file endpoints
// (/items/<key>/file/view/url), complementing `items open` (which launches the app)
// by handing an agent/script the actual file path to read or process.

package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/client"
)

func newItemsFileCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "file <itemKey>",
		Short: "Resolve the on-disk path (file:// URL) of an item's attachment",
		Long: `Return the local filesystem location of an item's attachment via the Zotero
local API (/items/<key>/file/view/url). Accepts an attachment key directly, or a
regular item key whose PDF attachment is resolved automatically through its child
items.

This complements 'items open' (which launches the Zotero desktop app): 'items file'
hands you the actual file so an agent or script can read or process it. The local
API must be enabled (Settings → Advanced → "Allow other applications…").`,
		Example: `  # Path of an item's PDF
  zotio items file ABCD1234

  # JSON envelope (item key, resolved attachment key, url, path)
  zotio items file ABCD1234 --json`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			itemKey := args[0]
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			attKey, fileURL, err := resolveAttachmentFileURL(c, itemKey)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			if fileURL == "" {
				return fmt.Errorf("no attachment file found for item %s", itemKey)
			}

			path := fileURLToPath(fileURL)
			if flags.asJSON {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"item_key":       itemKey,
					"attachment_key": attKey,
					"url":            fileURL,
					"path":           path,
				}, flags)
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	}
	return cmd
}

// resolveAttachmentFileURL returns the attachment key and its file URL for an
// item. It first treats itemKey as an attachment directly; failing that, it
// resolves the item's PDF attachment via its child items. Returns an empty URL
// (no error) when no attachment file is available.
func resolveAttachmentFileURL(c *client.Client, itemKey string) (string, string, error) {
	// itemKey may itself be an attachment with a file.
	if fileURL, ok := fetchAttachmentFileURL(c, itemKey); ok {
		return itemKey, fileURL, nil
	}
	// Otherwise resolve the item's PDF attachment among its children.
	// PATCH(glean zotero-pp-cli-1b05b22e1aeb8dd6): encode the user-supplied key as one Zotero path segment.
	childrenPath := replacePathParam("/items/{itemKey}/children", "itemKey", url.PathEscape(itemKey))
	children, err := c.Get(childrenPath, nil)
	if err != nil {
		return "", "", err
	}
	pdfKey, err := findPDFAttachmentKey(children)
	if err != nil {
		return "", "", err
	}
	if pdfKey == "" {
		return "", "", nil
	}
	fileURL, _ := fetchAttachmentFileURL(c, pdfKey)
	return pdfKey, fileURL, nil
}

// fetchAttachmentFileURL GETs the local-API file URL for an attachment key. The
// endpoint returns the file:// URL as plain text. A request error (e.g. the key
// is a regular item with no file) reports ok=false so the caller can fall back.
func fetchAttachmentFileURL(c *client.Client, key string) (string, bool) {
	// PATCH(glean zotero-pp-cli-1b05b22e1aeb8dd6): encode the attachment key as one Zotero path segment.
	path := replacePathParam("/items/{key}/file/view/url", "key", url.PathEscape(key))
	data, err := c.Get(path, nil)
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", false
	}
	// PATCH(glean zotero-pp-cli-777199d613c05bfd): tolerate local API builds that return the URL as a quoted JSON string.
	if strings.HasPrefix(s, `"`) {
		var quoted string
		if err := json.Unmarshal([]byte(s), &quoted); err == nil {
			s = strings.TrimSpace(quoted)
		}
	}
	return s, true
}

// fileURLToPath converts a file:// URL to a filesystem path, percent-decoding it.
// Non-file URLs (e.g. a linked web attachment) are returned unchanged.
func fileURLToPath(u string) string {
	// PATCH(glean zotero-pp-cli-777199d613c05bfd): normalize quoted URL strings even when callers bypass fetchAttachmentFileURL.
	if strings.HasPrefix(u, `"`) {
		var quoted string
		if err := json.Unmarshal([]byte(u), &quoted); err == nil {
			u = strings.TrimSpace(quoted)
		}
	}
	if !strings.HasPrefix(u, "file://") {
		return u
	}
	p := strings.TrimPrefix(u, "file://")
	// Drop an empty authority component: file:///Users/x -> /Users/x.
	if decoded, err := url.PathUnescape(p); err == nil {
		return decoded
	}
	return p
}
