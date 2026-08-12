package har

import (
	"crypto/sha256"
	"fmt"
)

// BodyStore holds decoded response bodies keyed by a truncated SHA-256 of
// their bytes, so identical bodies are stored once and referenced by hash.
// References look like "body:<first 16 hex chars of the sha256>".
type BodyStore struct {
	// ponytail: plain maps — the server is a single-threaded stdio process;
	// add a mutex if an HTTP mode or any concurrency ever lands.
	bodies map[string][]byte
	sizes  map[string]int64
	mimes  map[string]string
}

// NewBodyStore creates an empty body store.
func NewBodyStore() *BodyStore {
	return &BodyStore{
		bodies: make(map[string][]byte),
		sizes:  make(map[string]int64),
		mimes:  make(map[string]string),
	}
}

// Add stores body under a content-hash reference and returns the reference.
// Empty bodies are not stored and yield an empty reference. Storing an
// already-known body is a no-op: dedup is free because the key is the hash.
func (s *BodyStore) Add(body []byte, mimeType string) string {
	if len(body) == 0 {
		return ""
	}
	hash := sha256.Sum256(body)
	ref := fmt.Sprintf("body:%x", hash[:8])
	if _, ok := s.bodies[ref]; !ok {
		s.bodies[ref] = body
		s.sizes[ref] = int64(len(body))
		s.mimes[ref] = mimeType
	}
	return ref
}

// Get returns the stored body, its decoded size and mime type for a
// reference. The bool reports whether the reference is known.
func (s *BodyStore) Get(ref string) ([]byte, int64, string, bool) {
	body, ok := s.bodies[ref]
	if !ok {
		return nil, 0, "", false
	}
	return body, s.sizes[ref], s.mimes[ref], true
}
