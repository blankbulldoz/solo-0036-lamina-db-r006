package transport

import (
	"lamina/internal/catalog"
	"net/http"
)

func (s *Server) catalog(w http.ResponseWriter, r *http.Request) {
	relations, indexes, err := s.db.Relations()
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, 200, map[string]any{"relations": relations, "indexes": indexes})
}
func (s *Server) createRelation(w http.ResponseWriter, r *http.Request) {
	var in catalog.Relation
	if !decode(w, r, &in) {
		return
	}
	out, err := s.db.CreateRelation(in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, 201, out)
}
func (s *Server) createIndex(w http.ResponseWriter, r *http.Request) {
	var in catalog.Index
	if !decode(w, r, &in) {
		return
	}
	out, err := s.db.CreateIndex(in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, 201, out)
}
func (s *Server) evolveRelation(w http.ResponseWriter, r *http.Request) {
	var in catalog.EvolveRequest
	if !decode(w, r, &in) {
		return
	}
	out, err := s.db.EvolveRelation(in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, 200, out)
}
