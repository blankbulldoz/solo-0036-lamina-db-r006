package index

import (
	"lamina/internal/catalog"
	"lamina/internal/versions"
)

type Registry struct{ Trees map[string]*Tree }

func Build(c *catalog.Catalog, s *versions.Store) *Registry {
	r := &Registry{Trees: map[string]*Tree{}}
	for name, definition := range c.Indexes {
		tree := New()
		relation, ok := c.Relations[definition.Relation]
		if !ok {
			r.Trees[name] = tree
			continue
		}
		relation.EnsureRevisions()
		for key, chain := range s.Tables[definition.Relation] {
			for _, v := range chain {
				if v.Deleted {
					continue
				}
				// Historical rows may predate a later-added indexed column;
				// project them through the current schema (with defaults).
				row, err := relation.Interpret(v.Row)
				if err != nil {
					continue
				}
				tree.Add(row[definition.Column], key)
			}
		}
		r.Trees[name] = tree
	}
	return r
}
func (r *Registry) Apply(c *catalog.Catalog, writes []versions.Mutation) {
	for _, m := range writes {
		if m.Deleted {
			continue
		}
		for name, i := range c.Indexes {
			if i.Relation == m.Relation {
				r.Trees[name].Add(m.Row[i.Column], m.Key)
			}
		}
	}
}
