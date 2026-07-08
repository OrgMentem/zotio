// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Zotero desktop connector write client for local-first creates and stored attachments.

package connector

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const defaultAPIVersion = "3"

// Client speaks Zotero's desktop Connector API at /connector/*.
type Client struct {
	BaseURL    string
	HTTP       *http.Client
	APIVersion string
}

// New returns a Connector API client rooted at baseURL, usually
// http://localhost:23119/connector.
func New(baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTP:       &http.Client{Timeout: timeout},
		APIVersion: defaultAPIVersion,
	}
}

// NewID returns a random 16-byte hex identifier suitable for a connector session
// ID or item connector key.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating connector id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Ping returns nil iff the desktop connector endpoint is reachable.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/ping"), nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("connector ping: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("connector ping: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// SaveItems creates one or more items in the running Zotero desktop session.
// Each item must carry a unique connector-local "id" field.
func (c *Client) SaveItems(ctx context.Context, sessionID, uri string, items []map[string]any) error {
	payload := map[string]any{
		"sessionID": sessionID,
		"uri":       uri,
		"items":     items,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding connector saveItems request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/saveItems"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setVersion(req)
	req.Header.Set("Content-Type", "application/json")
	return c.expectCreated(req, "saveItems")
}

// SaveAttachment imports raw attachment bytes as a stored child of a connector-created parent.
func (c *Client) SaveAttachment(ctx context.Context, sessionID, parentConnectorKey, title, sourceURL, contentType string, data []byte) error {
	metadata := map[string]any{
		"sessionID":    sessionID,
		"parentItemID": parentConnectorKey,
		"title":        title,
		"url":          sourceURL,
	}
	meta, err := metadataHeaderJSON(metadata)
	if err != nil {
		return fmt.Errorf("encoding connector attachment metadata: %w", err)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/saveAttachment"), bytes.NewReader(data))
	if err != nil {
		return err
	}
	c.setVersion(req)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Metadata", meta)
	return c.expectCreated(req, "saveAttachment")
}

// SaveStandaloneAttachment imports raw attachment bytes and asks Zotero to recognize
// the file into a parent item when possible.
func (c *Client) SaveStandaloneAttachment(ctx context.Context, sessionID, title, sourceURL, contentType string, data []byte) (bool, error) {
	metadata := map[string]any{
		"sessionID": sessionID,
		"title":     title,
		"url":       sourceURL,
	}
	meta, err := metadataHeaderJSON(metadata)
	if err != nil {
		return false, fmt.Errorf("encoding connector standalone attachment metadata: %w", err)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/saveStandaloneAttachment"), bytes.NewReader(data))
	if err != nil {
		return false, err
	}
	c.setVersion(req)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Metadata", meta)
	body, err := c.expectStatus(req, "saveStandaloneAttachment", http.StatusCreated)
	if err != nil {
		return false, err
	}
	var payload struct {
		CanRecognize bool `json:"canRecognize"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			return false, fmt.Errorf("decoding connector saveStandaloneAttachment response: %w", err)
		}
	}
	return payload.CanRecognize, nil
}

// RecognizedItem is the metadata Zotero returns after standalone-PDF recognition.
type RecognizedItem struct {
	Title    string `json:"title"`
	ItemType string `json:"itemType"`
}

// GetRecognizedItem waits for Zotero's recognizer to finish. A 204 response means
// Zotero saved the standalone attachment but could not create a parent item.
func (c *Client) GetRecognizedItem(ctx context.Context, sessionID string) (RecognizedItem, bool, error) {
	payload := map[string]any{"sessionID": sessionID}
	body, err := json.Marshal(payload)
	if err != nil {
		return RecognizedItem{}, false, fmt.Errorf("encoding connector recognized item request: %w", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/getRecognizedItem"), bytes.NewReader(body))
	if err != nil {
		return RecognizedItem{}, false, err
	}
	c.setVersion(req)
	req.Header.Set("Content-Type", "application/json")
	httpClient := c.httpClient()
	httpClient.Timeout = 0
	resp, err := httpClient.Do(req)
	if err != nil {
		return RecognizedItem{}, false, fmt.Errorf("connector getRecognizedItem: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var item RecognizedItem
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&item); err != nil {
			return RecognizedItem{}, false, fmt.Errorf("decoding connector getRecognizedItem response: %w", err)
		}
		return item, true, nil
	case http.StatusNoContent:
		return RecognizedItem{}, false, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return RecognizedItem{}, false, fmt.Errorf("connector getRecognizedItem: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// HasAttachmentResolvers reports whether Zotero can fetch an attachment for a
// same-session connector-created item through its configured resolvers.
func (c *Client) HasAttachmentResolvers(ctx context.Context, sessionID, itemID string) (bool, error) {
	payload := map[string]any{"sessionID": sessionID, "itemID": itemID}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("encoding connector hasAttachmentResolvers request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/hasAttachmentResolvers"), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	c.setVersion(req)
	req.Header.Set("Content-Type", "application/json")
	respBody, err := c.expectStatus(req, "hasAttachmentResolvers", http.StatusOK)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(respBody)) == "true", nil
}

// SaveAttachmentFromResolver asks Zotero to download and attach a resolver-found
// file (for example an open-access PDF) to a same-session connector-created item.
func (c *Client) SaveAttachmentFromResolver(ctx context.Context, sessionID, itemID string) (string, error) {
	payload := map[string]any{"sessionID": sessionID, "itemID": itemID}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encoding connector saveAttachmentFromResolver request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/saveAttachmentFromResolver"), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	c.setVersion(req)
	req.Header.Set("Content-Type", "application/json")
	respBody, err := c.expectStatus(req, "saveAttachmentFromResolver", http.StatusCreated)
	if err != nil {
		return "", err
	}
	var title string
	if err := json.Unmarshal(respBody, &title); err == nil {
		return title, nil
	}
	return strings.TrimSpace(string(respBody)), nil
}

// Import sends raw bibliographic content (RIS/BibTeX/RDF/etc.) to Zotero's
// desktop import translators. Zotero returns the created items, including real
// item keys, when the import succeeds.
func (c *Client) Import(ctx context.Context, sessionID string, data []byte, contentType string) ([]json.RawMessage, error) {
	if contentType == "" {
		contentType = "text/plain"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/import")+"?session="+url.QueryEscape(sessionID), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	c.setVersion(req)
	req.Header.Set("Content-Type", contentType)
	body, err := c.expectStatus(req, "import", http.StatusCreated)
	if err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("decoding connector import response: %w", err)
	}
	return items, nil
}

// SelectedTarget is one desktop save destination returned by getSelectedCollection.
type SelectedTarget struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Level         int    `json:"level"`
	FilesEditable bool   `json:"filesEditable"`
}

// SelectedCollection describes Zotero desktop's current save target and target tree.
type SelectedCollection struct {
	LibraryID      int              `json:"libraryID"`
	LibraryName    string           `json:"libraryName"`
	Editable       bool             `json:"editable"`
	FilesEditable  bool             `json:"filesEditable"`
	SelectedID     any              `json:"id"`
	SelectedName   string           `json:"name"`
	Targets        []SelectedTarget `json:"targets"`
	TranslatorMode bool             `json:"translatorMode"`
}

// SelectedCollection returns Zotero desktop's editable save target tree.
func (c *Client) SelectedCollection(ctx context.Context) (SelectedCollection, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/getSelectedCollection"), bytes.NewReader([]byte("{}")))
	if err != nil {
		return SelectedCollection{}, err
	}
	c.setVersion(req)
	req.Header.Set("Content-Type", "application/json")
	body, err := c.expectStatus(req, "getSelectedCollection", http.StatusOK)
	if err != nil {
		return SelectedCollection{}, err
	}
	var selected SelectedCollection
	if err := json.Unmarshal(body, &selected); err != nil {
		return SelectedCollection{}, fmt.Errorf("decoding connector getSelectedCollection response: %w", err)
	}
	return selected, nil
}

// UpdateSession changes the target/tags/note for all items created in a connector
// session. Empty target, tags, and note fields are omitted.
func (c *Client) UpdateSession(ctx context.Context, sessionID, target string, tags []string, note string) error {
	payload := map[string]any{"sessionID": sessionID}
	if target != "" {
		payload["target"] = target
	}
	if len(tags) > 0 {
		payload["tags"] = tags
	}
	if note != "" {
		payload["note"] = note
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding connector updateSession request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/updateSession"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setVersion(req)
	req.Header.Set("Content-Type", "application/json")
	_, err = c.expectStatus(req, "updateSession", http.StatusOK)
	return err
}

// Translator describes one Zotero desktop translator returned by connector
// translator diagnostic endpoints.
type Translator struct {
	TranslatorID   string `json:"translatorID,omitempty"`
	Label          string `json:"label,omitempty"`
	Creator        string `json:"creator,omitempty"`
	Target         string `json:"target,omitempty"`
	TranslatorType int    `json:"translatorType,omitempty"`
	Priority       int    `json:"priority,omitempty"`
	InRepository   bool   `json:"inRepository,omitempty"`
	LastUpdated    string `json:"lastUpdated,omitempty"`
}

// GetTranslators lists available Zotero translators.
func (c *Client) GetTranslators(ctx context.Context) ([]Translator, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/getTranslators"), bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	c.setVersion(req)
	req.Header.Set("Content-Type", "application/json")
	body, err := c.expectStatus(req, "getTranslators", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var translators []Translator
	if err := json.Unmarshal(body, &translators); err != nil {
		return nil, fmt.Errorf("decoding connector getTranslators response: %w", err)
	}
	return translators, nil
}

// DetectTranslators asks Zotero which web translators match the provided page
// URL + HTML. It does not run the translators; browser-side translation is out of
// scope for the desktop connector server.
func (c *Client) DetectTranslators(ctx context.Context, pageURL, html string) ([]Translator, error) {
	payload := map[string]any{"uri": pageURL, "html": html}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding connector detect request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/detect"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setVersion(req)
	req.Header.Set("Content-Type", "application/json")
	respBody, err := c.expectStatus(req, "detect", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var translators []Translator
	if err := json.Unmarshal(respBody, &translators); err != nil {
		return nil, fmt.Errorf("decoding connector detect response: %w", err)
	}
	return translators, nil
}

func (c *Client) endpoint(path string) string {
	return strings.TrimRight(c.BaseURL, "/") + path
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) setVersion(req *http.Request) {
	version := c.APIVersion
	if version == "" {
		version = defaultAPIVersion
	}
	req.Header.Set("X-Zotero-Connector-API-Version", version)
}

func (c *Client) expectCreated(req *http.Request, endpoint string) error {
	_, err := c.expectStatus(req, endpoint, http.StatusCreated)
	return err
}

func metadataHeaderJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.Grow(len(data))
	for _, r := range string(data) {
		if r < utf8.RuneSelf {
			b.WriteByte(byte(r)) //nolint:gosec // G115: guarded by r < utf8.RuneSelf, so r fits in a byte.
			continue
		}
		if r <= 0xffff {
			fmt.Fprintf(&b, "\\u%04x", r)
			continue
		}
		hi, lo := utf16.EncodeRune(r)
		fmt.Fprintf(&b, "\\u%04x\\u%04x", hi, lo)
	}
	return b.String(), nil
}

func (c *Client) expectStatus(req *http.Request, endpoint string, want int) ([]byte, error) {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("connector %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	var body []byte
	if resp.StatusCode == want {
		body, _ = io.ReadAll(resp.Body)
	} else {
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	}
	if resp.StatusCode != want {
		return nil, fmt.Errorf("connector %s: HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}
