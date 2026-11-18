package engine

import (
	"lamina/internal/catalog"
	"lamina/internal/datum"
	"lamina/internal/index"
	"lamina/internal/journal"
)

func (d *DB) unique(table string, writes map[string]map[string]staged) error {
	relation, err := d.catalog.Relation(table)
	if err != nil {
		return invalid(err)
	}
	rows := d.interpretedScan(relation, d.sequence)
	overlay(rows, writes, table)
	for _, i := range d.catalog.Indexes {
		if i.Relation == table && i.Unique {
			if err := checkUnique(rows, i); err != nil {
				return err
			}
		}
	}
	return nil
}

// interpretedScan returns visible rows re-projected onto the schema effective
// at `at`, so defaulted columns added by later evolutions are present.
func (d *DB) interpretedScan(relation catalog.Relation, at uint64) map[string]datum.Row {
	schema, ok := relation.SchemaAt(at)
	if !ok {
		schema = relation
	}
	raw := d.store.Scan(relation.Name, at)
	rows := make(map[string]datum.Row, len(raw))
	for key, row := range raw {
		interpreted, err := schema.Interpret(row)
		if err != nil {
			continue
		}
		rows[key] = interpreted
	}
	return rows
}

func checkUnique(rows map[string]datum.Row, i catalog.Index) error {
	values := map[string]string{}
	for key, row := range rows {
		v := row[i.Column]
		if v.Null() {
			continue
		}
		token := v.Key()
		if prior, exists := values[token]; exists && prior != key {
			return fault("conflict", "unique constraint %s violated", i.Name)
		}
		values[token] = key
	}
	return nil
}
func (d *DB) CreateRelation(r catalog.Relation) (catalog.Relation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.usable(); err != nil {
		return r, err
	}
	if err := r.Validate(); err != nil {
		return r, invalid(err)
	}
	if _, exists := d.catalog.Relations[r.Name]; exists {
		return r, fault("conflict", "relation exists")
	}
	r.Created = d.sequence + 1
	r.Revisions = nil
	r.EnsureRevisions()
	if err := d.publish(journal.Record{Kind: "relation", Relation: &r}); err != nil {
		return r, err
	}
	return r, nil
}

// EvolveRelation applies one atomic, durable schema evolution. Shape errors
// are rejected as invalid input; semantic incompatibilities get the dedicated
// incompatible code so callers can tell rejected adjustments apart. No
// rejected request consumes a journal sequence.
func (d *DB) EvolveRelation(req catalog.EvolveRequest) (catalog.Relation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.usable(); err != nil {
		return catalog.Relation{}, err
	}
	if err := catalog.ValidateChangeShape(req); err != nil {
		return catalog.Relation{}, invalid(err)
	}
	r, err := d.catalog.Relation(req.Relation)
	if err != nil {
		return catalog.Relation{}, invalid(err)
	}
	columns, err := catalog.ApplyChanges(r, req.Changes)
	if err != nil {
		d.incompatible++
		return catalog.Relation{}, fault("incompatible", "%s", err)
	}
	record := journal.Record{Kind: "evolve", Evolve: &req, Columns: columns}
	if err = d.publish(record); err != nil {
		return catalog.Relation{}, err
	}
	d.evolutions++
	return d.catalog.Relations[req.Relation], nil
}

func (d *DB) CreateIndex(i catalog.Index) (catalog.Index, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.usable(); err != nil {
		return i, err
	}
	if err := d.catalog.CheckIndex(i); err != nil {
		return i, invalid(err)
	}
	if i.Unique {
		relation, err := d.catalog.Relation(i.Relation)
		if err != nil {
			return i, invalid(err)
		}
		if err := checkUnique(d.interpretedScan(relation, d.sequence), i); err != nil {
			return i, err
		}
	}
	i.Created = d.sequence + 1
	if err := d.publish(journal.Record{Kind: "index", Index: &i}); err != nil {
		return i, err
	}
	d.indexes = index.Build(d.catalog, d.store)
	return i, nil
}
