package http

import (
	"encoding/base64"
	"io"
	"math/rand"
	"net/http"
	"path"
	"sync"

	"github.com/reearth/ygo/crdt"
)

// maxUpdateBytes is the maximum accepted size for a POST body or sv parameter
// payload. Requests exceeding this limit are rejected before being read into
// memory, preventing OOM from large crafted uploads.
const maxUpdateBytes int64 = 64 << 20 // 64 MiB

// maxSVParamBytes is the maximum accepted length (bytes) of the base64-encoded
// sv query parameter. A state vector cannot realistically exceed a few KB; this
// cap prevents wasted allocation from oversized query strings.
const maxSVParamBytes = 1 << 16 // 64 KiB

// Server is an http.Handler that serves Yjs document sync over HTTP.
// Each distinct {room} path segment maps to a separate in-memory document.
//
// Mount it with a path pattern that captures the room name, e.g.:
//
//	http.Handle("/doc/{room}", srv)
type Server struct {
	mu   sync.RWMutex
	docs map[string]*crdt.Doc
}

// NewServer returns a new Server with an empty document store.
func NewServer() *Server {
	return &Server{
		docs: make(map[string]*crdt.Doc),
	}
}

// getOrCreateDoc returns the document for the given room, creating a new one
// with a random client ID if it does not exist.
func (s *Server) getOrCreateDoc(room string) *crdt.Doc {
	s.mu.Lock()
	defer s.mu.Unlock()
	if doc, ok := s.docs[room]; ok {
		return doc
	}
	// Use Uint32 to stay within Yjs wire protocol's 53-bit VarUint limit and
	// match crdt/doc.go's default generation (N-M1).
	doc := crdt.New(crdt.WithClientID(crdt.ClientID(rand.Uint32())))
	s.docs[room] = doc
	return doc
}

// GetDoc returns the document for the given room, or nil if it does not exist.
func (s *Server) GetDoc(room string) *crdt.Doc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.docs[room]
}

// ServeHTTP handles GET and POST requests.
//
// GET  /.../{room}?sv=<base64>  — returns binary V1 update diff (Content-Type: application/octet-stream)
// POST /.../{room}              — body is a binary V1 update; applies it to the room doc
//
// Room name is extracted from the last path segment (PathValue("room") on
// Go 1.22+, or the final segment of r.URL.Path as fallback).
//
// GET with no sv parameter returns the full document state.
// A 400 is returned if the sv parameter is present but cannot be base64-decoded
// or parsed as a state vector.
// A 400 is returned if the POST body cannot be applied.
// A 405 is returned for any other method.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract room name: try PathValue first (Go 1.22 ServeMux), fall back to path.Base.
	room := r.PathValue("room")
	if room == "" {
		room = path.Base(r.URL.Path)
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGet(w, r, room)
	case http.MethodPost:
		s.handlePost(w, r, room)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, room string) {
	doc := s.getOrCreateDoc(room)

	var sv crdt.StateVector

	svParam := r.URL.Query().Get("sv")
	if svParam != "" {
		if int64(len(svParam)) > maxSVParamBytes {
			http.Error(w, "invalid sv parameter: too large", http.StatusBadRequest)
			return
		}
		svBytes, err := base64.StdEncoding.DecodeString(svParam)
		if err != nil {
			http.Error(w, "invalid sv parameter: base64 decode failed", http.StatusBadRequest)
			return
		}
		var parseErr error
		sv, parseErr = crdt.DecodeStateVectorV1(svBytes)
		if parseErr != nil {
			http.Error(w, "invalid sv parameter: state vector parse failed", http.StatusBadRequest)
			return
		}
	}

	update := crdt.EncodeStateAsUpdateV1(doc, sv)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(update)
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, room string) {
	// Require application/octet-stream to prevent browsers from accidentally
	// POSTing JSON or form-encoded data that would fail with a cryptic decode error.
	if ct := r.Header.Get("Content-Type"); ct != "application/octet-stream" {
		http.Error(w, "unsupported media type: Content-Type must be application/octet-stream", http.StatusUnsupportedMediaType)
		return
	}

	// Reject bodies exceeding maxUpdateBytes before buffering the entire payload.
	r.Body = http.MaxBytesReader(w, r.Body, maxUpdateBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusRequestEntityTooLarge)
		return
	}

	doc := s.getOrCreateDoc(room)

	if err := crdt.ApplyUpdateV1(doc, body, r.RemoteAddr); err != nil {
		// Return a generic message — err.Error() may contain internal decoder
		// details that are unhelpful to callers and leak implementation info (N-M4).
		http.Error(w, "failed to apply update", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
