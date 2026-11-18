package engine

import (
	"fmt"
	"lamina/internal/catalog"
	"lamina/internal/datum"
	"lamina/internal/journal"
)

func (d *DB) apply(r journal.Record) error {
	if r.Sequence != d.sequence+1 || r.Format != 1 {
		return fmt.Errorf("invalid record sequence")
	}
	switch r.Kind {
	case "relation":
		if r.Relation == nil || r.Relation.Created != r.Sequence {
			return fmt.Errorf("invalid relation record")
		}
		if err := r.Relation.Validate(); err != nil {
			return err
		}
		if _, ok := d.catalog.Relations[r.Relation.Name]; ok {
			return fmt.Errorf("duplicate relation")
		}
		relation := *r.Relation
		relation.EnsureRevisions()
		if relation.Revisions[0].Sequence != r.Sequence {
			return fmt.Errorf("invalid initial schema revision")
		}
		d.catalog.Relations[relation.Name] = relation
		d.store.Ensure(relation.Name)
	case "evolve":
		if r.Evolve == nil || len(r.Columns) == 0 {
			return fmt.Errorf("invalid evolve record")
		}
		relation, ok := d.catalog.Relations[r.Evolve.Relation]
		if !ok {
			return fmt.Errorf("evolve references unknown relation")
		}
		relation.EnsureRevisions()
		columns, err := catalog.ApplyChanges(relation, r.Evolve.Changes)
		if err != nil {
			return err
		}
		if !sameColumns(columns, r.Columns) {
			return fmt.Errorf("evolve record does not match its durable schema")
		}
		relation.ApplyRevision(r.Sequence, "evolve", r.Evolve.Note, columns)
		d.catalog.Relations[relation.Name] = relation
	case "index":
		if r.Index == nil || r.Index.Created != r.Sequence {
			return fmt.Errorf("invalid index record")
		}
		if err := d.catalog.CheckIndex(*r.Index); err != nil {
			return err
		}
		d.catalog.Indexes[r.Index.Name] = *r.Index
	case "commit":
		seen := map[string]bool{}
		for _, m := range r.Writes {
			relation, err := d.catalog.Relation(m.Relation)
			if err != nil {
				return err
			}
			token := m.Relation + "\x00" + m.Key
			if seen[token] {
				return fmt.Errorf("duplicate mutation")
			}
			seen[token] = true
			if !m.Deleted {
				// Bind to the revision effective when the commit was durable:
				// historical rows must keep their historical interpretation.
				schema, ok := relation.SchemaAt(r.Sequence)
				if !ok {
					return fmt.Errorf("commit precedes relation schema")
				}
				row, err := schema.Normalize(m.Row)
				if err != nil {
					return err
				}
				if schema.Key(row) != m.Key {
					return fmt.Errorf("stored primary mismatch")
				}
			}
		}
		d.store.Apply(r.Sequence, r.Writes)
	case "vacuum":
		if r.Horizon < d.horizon || r.Horizon > d.sequence {
			return fmt.Errorf("invalid vacuum horizon")
		}
		d.store.Vacuum(r.Horizon)
		d.catalog.PruneRevisions(func(relation string) uint64 {
			return d.relationWatermark(relation, r.Horizon)
		})
		d.horizon = r.Horizon
	default:
		return fmt.Errorf("unknown record kind %s", r.Kind)
	}
	d.sequence = r.Sequence
	return nil
}

// relationWatermark returns the lowest snapshot that can still address a
// relation: the global horizon, raised to the oldest retained row when every
// retained version is newer. Revisions effective at or below this watermark
// are superseded by the revision still binding the watermark.
func (d *DB) relationWatermark(name string, fallback uint64) uint64 {
	oldest := uint64(0)
	for _, chain := range d.store.Tables[name] {
		if len(chain) == 0 {
			continue
		}
		seq := chain[0].Sequence
		if oldest == 0 || seq < oldest {
			oldest = seq
		}
	}
	if oldest > fallback {
		return oldest
	}
	return fallback
}

func sameColumns(a, b []catalog.Column) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Type != b[i].Type || a[i].Nullable != b[i].Nullable {
			return false
		}
		switch {
		case a[i].Default == nil && b[i].Default == nil:
		case a[i].Default == nil || b[i].Default == nil:
			return false
		default:
			if a[i].Default.Key() != b[i].Default.Key() {
				return false
			}
		}
	}
	return true
}

func overlay(rows map[string]datum.Row, writes map[string]map[string]staged, table string) {
	for key, m := range writes[table] {
		if m.Mutation.Deleted {
			delete(rows, key)
		} else {
			rows[key] = datum.Clone(m.Mutation.Row)
		}
	}
}
