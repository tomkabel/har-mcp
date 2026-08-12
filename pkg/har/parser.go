// Package har provides functionality for parsing and working with HAR (HTTP Archive) files.
package har

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/martian/har"
)

// Parser handles HAR file parsing from various sources. It holds the body
// store populated at parse time, so decoded response bodies can be fetched
// later by content hash.
type Parser struct {
	bodies *BodyStore
}

// NewParser creates a new HAR parser
func NewParser() *Parser {
	return &Parser{bodies: NewBodyStore()}
}

// ParseFromFile parses a HAR file from disk
func (p *Parser) ParseFromFile(path string) (*har.HAR, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open HAR file: %w", err)
	}
	defer file.Close() //nolint:errcheck

	return p.Parse(file)
}

// ParseFromURL parses a HAR file from an HTTP URL
func (p *Parser) ParseFromURL(harURL string) (*har.HAR, error) {
	resp, err := http.Get(harURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch HAR from URL: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch HAR: HTTP %d", resp.StatusCode)
	}

	return p.Parse(resp.Body)
}

// Parse parses a HAR file from the given reader
func (p *Parser) Parse(r io.Reader) (*har.HAR, error) {
	// Read all data so we can try multiple parsing approaches
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read HAR data: %w", err)
	}

	// First try standard parsing
	var harData har.HAR
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&harData); err == nil {
		// Standard parsing succeeded
		p.indexResponseBodies(&harData)
		return &harData, nil
	}

	// If standard parsing failed, try flexible parsing
	var flexibleHAR FlexibleHAR
	decoder = json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&flexibleHAR); err != nil {
		return nil, fmt.Errorf("failed to parse HAR file: %w", err)
	}

	// Convert flexible HAR to standard HAR
	standardHAR := flexibleHAR.ToStandardHAR()
	p.indexResponseBodies(standardHAR)
	return standardHAR, nil
}

// indexResponseBodies stores every non-empty decoded response body in the
// body store, deduplicated by content hash. This is the single choke point
// where bodies enter the store, covering both the standard decode and the
// FlexibleHAR fallback.
func (p *Parser) indexResponseBodies(harData *har.HAR) {
	for _, entry := range harData.Log.Entries {
		if entry.Response == nil || entry.Response.Content == nil {
			continue
		}
		p.bodies.Add(entry.Response.Content.Text, entry.Response.Content.MimeType)
	}
}

// URLMethodEntry represents a URL and method combination with associated request IDs
type URLMethodEntry struct {
	URL        string   `json:"url"`
	Method     string   `json:"method"`
	RequestIDs []string `json:"request_ids"`
}

// GetURLsAndMethods returns all unique URL and method combinations from the HAR
func (p *Parser) GetURLsAndMethods(harData *har.HAR) []URLMethodEntry {
	// Map to store unique URL+Method combinations and their request IDs
	urlMethodMap := make(map[string]*URLMethodEntry)

	for i, entry := range harData.Log.Entries {
		if entry.Request == nil {
			continue
		}

		key := fmt.Sprintf("%s|%s", entry.Request.URL, entry.Request.Method)
		requestID := fmt.Sprintf("request_%d", i)

		if existing, ok := urlMethodMap[key]; ok {
			existing.RequestIDs = append(existing.RequestIDs, requestID)
		} else {
			urlMethodMap[key] = &URLMethodEntry{
				URL:        entry.Request.URL,
				Method:     entry.Request.Method,
				RequestIDs: []string{requestID},
			}
		}
	}

	// Convert map to slice
	var result []URLMethodEntry
	for _, entry := range urlMethodMap {
		result = append(result, *entry)
	}

	return result
}

// GetRequestIDsForURLMethod returns all request IDs for a specific URL and method
func (p *Parser) GetRequestIDsForURLMethod(harData *har.HAR, targetURL, method string) []string {
	var requestIDs []string

	for i, entry := range harData.Log.Entries {
		if entry.Request == nil {
			continue
		}

		if entry.Request.URL == targetURL && entry.Request.Method == method {
			requestID := fmt.Sprintf("request_%d", i)
			requestIDs = append(requestIDs, requestID)
		}
	}

	return requestIDs
}

// RequestDetails represents the full details of a request with auth headers
// redacted and response bodies bounded to a preview. Raw bodies are never
// included: a huge response (e.g. a base64 mp4) would otherwise dump hundreds
// of thousands of tokens into the agent context on a single tool call.
type RequestDetails struct {
	RequestID       string        `json:"request_id"`
	StartedDateTime string        `json:"started_datetime"`
	Time            float64       `json:"time"`
	Request         *RequestInfo  `json:"request"`
	Response        *ResponseInfo `json:"response"`
	Cache           *har.Cache    `json:"cache,omitempty"`
	Timings         *har.Timings  `json:"timings,omitempty"`
	ServerIPAddress string        `json:"serverIPAddress,omitempty"`
	Connection      string        `json:"connection,omitempty"`
	Comment         string        `json:"comment,omitempty"`
}

// ResponseInfo is like har.Response but with redacted headers and a bounded
// body preview instead of the raw body.
type ResponseInfo struct {
	Status      int          `json:"status"`
	StatusText  string       `json:"statusText"`
	HTTPVersion string       `json:"httpVersion"`
	Cookies     []har.Cookie `json:"cookies"`
	Headers     []har.Header `json:"headers"`
	Content     *ContentInfo `json:"content"`
	RedirectURL string       `json:"redirectURL"`
	HeadersSize int64        `json:"headersSize"`
	BodySize    int64        `json:"bodySize"`
}

// ContentInfo is response content metadata with a bounded text preview. Binary
// content (video, images, fonts) carries metadata only — an LLM cannot read
// it, so shipping even a preview is token waste. Hash is the body-store
// reference for the full decoded body, fetched on demand via get_response_body.
type ContentInfo struct {
	Size        int64  `json:"size"`
	MimeType    string `json:"mimeType"`
	Encoding    string `json:"encoding,omitempty"`
	Hash        string `json:"hash,omitempty"`
	TextPreview string `json:"textPreview,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
}

// maxBodyPreview is the maximum number of body bytes included in a ContentInfo
// preview. This is what keeps get_request_details bounded regardless of the
// captured response size.
const maxBodyPreview = 4096

// RequestInfo is like har.Request but with redacted auth headers
type RequestInfo struct {
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	HTTPVersion string            `json:"httpVersion"`
	Cookies     []har.Cookie      `json:"cookies"`
	Headers     []har.Header      `json:"headers"`
	QueryString []har.QueryString `json:"queryString"`
	PostData    *har.PostData     `json:"postData,omitempty"`
	HeadersSize int64             `json:"headersSize"`
	BodySize    int64             `json:"bodySize"`
}

// GetRequestDetails returns the full details of a request by ID with auth headers redacted
func (p *Parser) GetRequestDetails(harData *har.HAR, requestID string) (*RequestDetails, error) {
	// Extract index from request ID
	var index int
	if _, err := fmt.Sscanf(requestID, "request_%d", &index); err != nil {
		return nil, fmt.Errorf("invalid request ID format: %s", requestID)
	}

	if index < 0 || index >= len(harData.Log.Entries) {
		return nil, fmt.Errorf("request ID out of range: %s", requestID)
	}

	entry := harData.Log.Entries[index]

	// Create request info with redacted headers
	requestInfo := &RequestInfo{
		Method:      entry.Request.Method,
		URL:         entry.Request.URL,
		HTTPVersion: entry.Request.HTTPVersion,
		Cookies:     entry.Request.Cookies,
		Headers:     p.redactAuthHeaders(entry.Request.Headers),
		QueryString: entry.Request.QueryString,
		PostData:    entry.Request.PostData,
		HeadersSize: entry.Request.HeadersSize,
		BodySize:    entry.Request.BodySize,
	}

	details := &RequestDetails{
		RequestID:       requestID,
		StartedDateTime: entry.StartedDateTime.Format(time.RFC3339),
		Time:            float64(entry.Time),
		Request:         requestInfo,
		Response:        p.buildResponseInfo(entry.Response),
		Cache:           entry.Cache,
		Timings:         entry.Timings,
	}

	return details, nil
}

// buildResponseInfo converts a har.Response into a ResponseInfo with redacted
// headers and a bounded text preview instead of the raw body.
func (p *Parser) buildResponseInfo(resp *har.Response) *ResponseInfo {
	if resp == nil {
		return nil
	}

	info := &ResponseInfo{
		Status:      resp.Status,
		StatusText:  resp.StatusText,
		HTTPVersion: resp.HTTPVersion,
		Cookies:     resp.Cookies,
		Headers:     p.redactAuthHeaders(resp.Headers),
		RedirectURL: resp.RedirectURL,
		HeadersSize: resp.HeadersSize,
		BodySize:    resp.BodySize,
	}

	if resp.Content != nil {
		info.Content = &ContentInfo{
			Size:     resp.Content.Size,
			MimeType: resp.Content.MimeType,
			Encoding: resp.Content.Encoding,
			// Add is idempotent: bodies already indexed at parse time are
			// returned by reference without re-storing.
			Hash: p.bodies.Add(resp.Content.Text, resp.Content.MimeType),
		}
		if isTextMime(resp.Content.MimeType) && len(resp.Content.Text) > 0 {
			preview := resp.Content.Text
			if len(preview) > maxBodyPreview {
				preview = preview[:maxBodyPreview]
				info.Content.Truncated = true
			}
			info.Content.TextPreview = string(preview)
		}
	}

	return info
}

// isTextMime reports whether a mime type is likely to carry LLM-readable text.
// Binary mimes (video/*, image/*, font/*, ...) get metadata only.
func isTextMime(mime string) bool {
	mime = strings.ToLower(strings.SplitN(mime, ";", 2)[0])
	return strings.HasPrefix(mime, "text/") ||
		mime == "application/json" ||
		mime == "application/xml" ||
		mime == "application/javascript" ||
		mime == "application/x-javascript" ||
		strings.HasSuffix(mime, "+json") ||
		strings.HasSuffix(mime, "+xml")
}

// redactAuthHeaders redacts sensitive authentication headers
func (p *Parser) redactAuthHeaders(headers []har.Header) []har.Header {
	authHeaders := map[string]bool{
		"authorization":       true,
		"x-api-key":           true,
		"x-auth-token":        true,
		"cookie":              true,
		"set-cookie":          true,
		"proxy-authorization": true,
	}

	redactedHeaders := make([]har.Header, len(headers))
	for i, header := range headers {
		redactedHeaders[i] = har.Header{
			Name:  header.Name,
			Value: header.Value,
		}

		if authHeaders[strings.ToLower(header.Name)] {
			redactedHeaders[i].Value = "[REDACTED]"
		}
	}

	return redactedHeaders
}

// ParseSource parses a HAR file from either a file path or URL
func (p *Parser) ParseSource(source string) (*har.HAR, error) {
	// Check if it's a URL
	if u, err := url.Parse(source); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		return p.ParseFromURL(source)
	}

	// Otherwise treat as file path
	return p.ParseFromFile(source)
}

// BodyChunk is a bounded slice of a stored response body. Text bodies carry
// the chunk in Text; binary bodies carry metadata and a Note instead, because
// the bytes are not LLM-readable.
type BodyChunk struct {
	Hash      string `json:"hash"`
	Offset    int    `json:"offset"`
	Limit     int    `json:"limit"`
	TotalSize int64  `json:"totalSize"`
	Truncated bool   `json:"truncated"`
	Text      string `json:"text,omitempty"`
	MimeType  string `json:"mimeType"`
	Note      string `json:"note,omitempty"`
}

// defaultBodyChunk is the chunk size used when the caller omits limit.
const defaultBodyChunk = 4096

// maxBodyChunk caps a single chunk so a fetch cannot dump an unbounded body
// into the agent context.
const maxBodyChunk = 64 * 1024

// GetResponseBody returns a chunk of a stored response body between offset
// and offset+limit. Offset is clamped to 0; a missing or negative limit
// defaults to defaultBodyChunk and is capped at maxBodyChunk. Binary bodies
// return metadata only — the bytes are never shipped.
func (p *Parser) GetResponseBody(ref string, offset, limit int) (*BodyChunk, error) {
	body, totalSize, mime, ok := p.bodies.Get(ref)
	if !ok {
		return nil, fmt.Errorf("unknown body hash: %s", ref)
	}

	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = defaultBodyChunk
	}
	if limit > maxBodyChunk {
		limit = maxBodyChunk
	}

	chunk := &BodyChunk{
		Hash:      ref,
		Offset:    offset,
		Limit:     limit,
		TotalSize: totalSize,
		MimeType:  mime,
	}

	if !isTextMime(mime) {
		chunk.Note = "binary body, not readable — metadata only"
		return chunk, nil
	}

	start := offset
	if start > len(body) {
		start = len(body)
	}
	end := start + limit
	if end > len(body) {
		end = len(body)
	}
	chunk.Text = string(body[start:end])
	chunk.Truncated = end < len(body)
	return chunk, nil
}
