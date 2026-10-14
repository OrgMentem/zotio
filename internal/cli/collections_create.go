// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/mutation"
)

func newCollectionsCreateCmd(flags *rootFlags) *cobra.Command {
	var bodyName string
	var bodyParentCollection string
	var stdinBody bool

	cmd := &cobra.Command{
		Use:     "create",
		Short:   "Create one or more collections",
		Example: "  zotio collections create --name \"Reading list\" --parent-collection ABCD2345",
		Annotations: map[string]string{
			"zotio:endpoint": "collections.create",
			"zotio:method":   "POST",
			"zotio:path":     "/collections",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Every flag and payload check runs before the preview branch, so a
			// preview never succeeds for input the apply refuses.
			body, err := collectionCreateBody(cmd, bodyName, bodyParentCollection, stdinBody)
			if err != nil {
				return err
			}
			path := "/collections"
			ops := collectionCreateOps(body)
			// Preview and apply run the same reads: every named parent collection
			// must exist on the write plane (404 is exit 3). A create without a
			// parent reads nothing, so its preview needs no client.
			parents := collectionCreateParents(body)
			var c *client.Client
			if len(parents) > 0 || resolveMutationMode(flags).Apply {
				if c, err = flags.newWriteClient(); err != nil {
					return err
				}
			}
			for _, parent := range parents {
				if _, err := readCollectionVersion(c, flags, parent); err != nil {
					return err
				}
			}
			// The Web API create is the only write below. Zotero answers a
			// batched POST with HTTP 200 even when it rejects some elements, and
			// the elements it did not reject were still created -- so wrapping
			// the whole batch in one mutation.Op (as before) let a single
			// rejected element erase the journal record of everything created
			// alongside it (recordMutationJournal skips runs with Applied == 0).
			// One Op per collection fixes that: the first collection's Apply
			// issues the single batched POST and caches the decoded response,
			// and every other collection reads its outcome from that cache, so
			// the HTTP request count is unchanged. A non-string Add means
			// applyChangeToItemData always refuses the replay -- a collection
			// key must never land on an item.
			var (
				applyErr       error // classified failure returned as the command's own error
				batchExecuted  bool
				batchTransport error // set only when the POST itself failed (not a per-element rejection)
				batchEnvelope  error // set when the 2xx body is not the batch envelope: no outcome is proven
				batchFailed    map[string]batchWriteFailure
				batchKeys      map[string]string
			)
			applyCollection := func(index int) (string, any, error) {
				if !batchExecuted {
					batchExecuted = true
					data, _, postErr := c.Post(path, body)
					if postErr != nil {
						batchTransport = classifyAPIError(postErr, flags)
						applyErr = batchTransport
					} else if envErr := checkBatchEnvelope(data, len(ops)); envErr != nil {
						// A 2xx without the batch envelope proves nothing: it may
						// be a proxy error page, a singleton object, or truncated
						// JSON. Decoding only Failed would read all of those as
						// zero failures and report every collection applied, so
						// the envelope must be verified first.
						batchEnvelope = fmt.Errorf("collections create: %w; the outcome of the %d collection(s) in that request is unknown", envErr, len(ops))
						applyErr = batchEnvelope
					} else {
						batchFailed = decodeBatchWriteResponse(data).Failed
						batchKeys = createdCollectionKeysByIndex(data)
						applyErr = batchWriteFailuresError("collections create", batchFailed)
					}
				}
				if batchTransport != nil {
					return "failed", nil, batchTransport
				}
				if batchEnvelope != nil {
					return "failed", batchEnvelope.Error(), batchEnvelope
				}
				if failure, ok := batchFailed[strconv.Itoa(index)]; ok {
					return "failed", failure.detail(strconv.Itoa(index)), nil
				}
				if key := batchKeys[strconv.Itoa(index)]; key != "" {
					return "applied", map[string]any{"key": key}, nil
				}
				return "applied", nil, nil
			}
			for i := range ops {
				index := i
				ops[i].Apply = func() (string, any, error) {
					return applyCollection(index)
				}
			}
			// A rejected element travelled to Zotero alongside its batch-mates
			// and cannot un-submit them, so stopping at the first rejection
			// would report the rest as not_attempted -- a lie that invites a
			// duplicating re-create. Every collection reports its own outcome.
			env, runErr := runMutation(cmd.Context(), flags, "collections.create", ops, func(o *mutation.Options) {
				o.ContinueOnError = true
			})
			return renderMutationResult(cmd, flags, env, preferApplyErr(runErr, applyErr), collectionWriteLine("create", "created"))
		},
	}
	cmd.Flags().StringVar(&bodyName, "name", "", "Collection name")
	cmd.Flags().StringVar(&bodyParentCollection, "parent-collection", "", "Parent collection key (omit for top-level)")
	cmd.Flags().BoolVar(&stdinBody, "stdin", false, "Read request body as JSON from stdin")

	return cmd
}

// collectionCreateBody validates the create flags or stdin payload and returns
// the collection objects to POST.
func collectionCreateBody(cmd *cobra.Command, name, parent string, stdinBody bool) ([]map[string]any, error) {
	nameSet, parentSet := cmd.Flags().Changed("name"), cmd.Flags().Changed("parent-collection")
	if stdinBody {
		if nameSet || parentSet {
			return nil, usageErr(fmt.Errorf("--stdin cannot be combined with --name or --parent-collection"))
		}
		stdinData, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, fmt.Errorf("reading stdin: %w", err)
		}
		var jsonBody any
		if err := json.Unmarshal(stdinData, &jsonBody); err != nil {
			return nil, usageErr(fmt.Errorf("parsing stdin JSON: %w", err))
		}
		var payloads []any
		switch payload := jsonBody.(type) {
		case map[string]any:
			payloads = []any{payload}
		case []any:
			payloads = payload
		default:
			return nil, usageErr(fmt.Errorf("parsing stdin JSON: collection payload must be an object or array of objects"))
		}
		if len(payloads) == 0 {
			return nil, usageErr(fmt.Errorf("parsing stdin JSON: collection payload is empty"))
		}
		collections := make([]map[string]any, 0, len(payloads))
		for i, payload := range payloads {
			fields, ok := payload.(map[string]any)
			if !ok {
				return nil, usageErr(fmt.Errorf("parsing stdin JSON: element %d is not an object", i))
			}
			if collName, _ := fields["name"].(string); strings.TrimSpace(collName) == "" {
				return nil, usageErr(fmt.Errorf("parsing stdin JSON: element %d needs a non-empty \"name\"", i))
			}
			collections = append(collections, fields)
		}
		return collections, nil
	}
	if !nameSet {
		return nil, usageErr(fmt.Errorf("required flag %q not set", "name"))
	}
	if strings.TrimSpace(name) == "" {
		return nil, usageErr(fmt.Errorf("--name must not be empty"))
	}
	collection := map[string]any{"name": name}
	if parent != "" {
		if err := validateKeyArg("collection", parent); err != nil {
			return nil, err
		}
		collection["parentCollection"] = parent
	}
	return []map[string]any{collection}, nil
}

// collectionCreateParents lists the distinct parent collection keys the new
// collections name.
func collectionCreateParents(body []map[string]any) []string {
	var parents []string
	seen := map[string]bool{}
	for _, collection := range body {
		if parent, _ := collection["parentCollection"].(string); parent != "" && !seen[parent] {
			seen[parent] = true
			parents = append(parents, parent)
		}
	}
	return parents
}

// createdCollectionKeysByIndex maps each request index of a batch create to the
// key Zotero assigned, from either response shape it uses.
func createdCollectionKeysByIndex(data []byte) map[string]string {
	var response struct {
		Success    map[string]string          `json:"success"`
		Successful map[string]json.RawMessage `json:"successful"`
	}
	keys := map[string]string{}
	if json.Unmarshal(data, &response) != nil {
		// success may hold non-string values in odd proxies; fall back to successful only.
		var alt struct {
			Successful map[string]json.RawMessage `json:"successful"`
		}
		if json.Unmarshal(data, &alt) != nil {
			return keys
		}
		response.Successful = alt.Successful
	}
	for index, raw := range response.Successful {
		var obj struct {
			Key string `json:"key"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.Key != "" {
			keys[index] = obj.Key
		}
	}
	for index, key := range response.Success {
		if key != "" {
			keys[index] = key
		}
	}
	return keys
}

func collectionCreateOps(body any) []mutation.Op {
	var collections []any
	switch payload := body.(type) {
	case []any:
		collections = payload
	case []map[string]any:
		collections = make([]any, len(payload))
		for i := range payload {
			collections[i] = payload[i]
		}
	default:
		return nil
	}

	ops := make([]mutation.Op, 0, len(collections))
	for i, collection := range collections {
		key := fmt.Sprintf("%d", i+1)
		if fields, ok := collection.(map[string]any); ok {
			if name, ok := fields["name"].(string); ok && name != "" {
				key = name
			}
		}
		ops = append(ops, mutation.Op{
			ID:      fmt.Sprintf("collections.create:%d", i+1),
			Key:     key,
			Kind:    "collection_create",
			Changes: []mutation.Change{{Field: "collection", Add: collection}},
		})
	}
	return ops
}
