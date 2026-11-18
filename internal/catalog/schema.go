package catalog

import (
	"fmt"
	"lamina/internal/datum"
	"regexp"
)

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func ValidName(s string) bool { return identifier.MatchString(s) }

type Column struct {
	Name     string       `json:"name"`
	Type     string       `json:"type"`
	Nullable bool         `json:"nullable"`
	Default  *datum.Value `json:"default,omitempty"`
}

// Revision is an immutable schema interpretation effective at Sequence.
// Rows written before Sequence stay readable through the previous revision;
// a snapshot at `at` is bound to the latest revision with Sequence <= at.
type Revision struct {
	Sequence uint64   `json:"sequence"`
	Action   string   `json:"action"`
	Note     string   `json:"note,omitempty"`
	Columns  []Column `json:"columns"`
}

type Relation struct {
	Name      string     `json:"name"`
	Primary   string     `json:"primary"`
	Columns   []Column   `json:"columns"`
	Created   uint64     `json:"created"`
	Revisions []Revision `json:"revisions"`
}

func validColumnType(t string) bool { return t == "int" || t == "text" || t == "bool" }

func defaultValid(c Column) error {
	if c.Default == nil || c.Default.Null() {
		return nil
	}
	if c.Default.Kind != c.Type {
		return fmt.Errorf("default for column %s must be %s", c.Name, c.Type)
	}
	if c.Type == "text" && len(c.Default.Text) > 65536 {
		return fmt.Errorf("default text exceeds 64KiB")
	}
	return nil
}

func (r Relation) Validate() error {
	if !ValidName(r.Name) || !ValidName(r.Primary) {
		return fmt.Errorf("invalid relation or primary name")
	}
	if len(r.Columns) == 0 || len(r.Columns) > 64 {
		return fmt.Errorf("relation needs 1..64 columns")
	}
	seen := map[string]bool{}
	found := false
	for _, c := range r.Columns {
		if !ValidName(c.Name) || seen[c.Name] {
			return fmt.Errorf("invalid or duplicate column %q", c.Name)
		}
		seen[c.Name] = true
		if !validColumnType(c.Type) {
			return fmt.Errorf("unsupported type %q", c.Type)
		}
		if err := defaultValid(c); err != nil {
			return err
		}
		if c.Name == r.Primary {
			found = true
			if c.Nullable || c.Type == "bool" {
				return fmt.Errorf("primary must be non-null int or text")
			}
			if c.Default != nil {
				return fmt.Errorf("primary column cannot carry a default")
			}
		}
	}
	if !found {
		return fmt.Errorf("primary column does not exist")
	}
	return nil
}

func (r Relation) Column(name string) (Column, bool) {
	for _, c := range r.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// EnsureRevisions backfills the initial revision for relations created before
// schema evolution existed (old journal records and checkpoint images).
func (r *Relation) EnsureRevisions() {
	if len(r.Revisions) > 0 {
		return
	}
	r.Revisions = []Revision{{Sequence: r.Created, Action: "create", Columns: cloneColumns(r.Columns)}}
}

func (r Relation) CurrentRevision() Revision {
	if len(r.Revisions) == 0 {
		return Revision{Sequence: r.Created, Action: "create", Columns: cloneColumns(r.Columns)}
	}
	return r.Revisions[len(r.Revisions)-1]
}

// RevisionAt returns the schema revision effective at snapshot at.
func (r Relation) RevisionAt(at uint64) (Revision, bool) {
	if len(r.Revisions) == 0 {
		return Revision{Sequence: r.Created, Action: "create", Columns: cloneColumns(r.Columns)}, at >= r.Created
	}
	pos := -1
	for i, rev := range r.Revisions {
		if rev.Sequence <= at {
			pos = i
		} else {
			break
		}
	}
	if pos < 0 {
		// No retained revision predates at: vacuum pruned the earlier ones.
		// The oldest retained revision is the schema that interpreted such
		// rows at the prune watermark, and columns are additive, so it stays
		// a valid projection. Only return it for snapshots inside retention.
		if at >= r.Created {
			return r.Revisions[0], true
		}
		return Revision{}, false
	}
	return r.Revisions[pos], true
}

// SchemaAt returns the relation as interpreted at snapshot at, with Columns
// bound to the effective revision. Callers must reject the false case as an
// unavailable snapshot rather than falling back to the current schema.
func (r Relation) SchemaAt(at uint64) (Relation, bool) {
	rev, ok := r.RevisionAt(at)
	if !ok {
		return Relation{}, false
	}
	out := r
	out.Columns = cloneColumns(rev.Columns)
	return out, true
}

// ApplyRevision appends a durable evolution revision and mirrors its columns
// as the current schema.
func (r *Relation) ApplyRevision(seq uint64, action, note string, columns []Column) {
	r.EnsureRevisions()
	r.Columns = cloneColumns(columns)
	r.Revisions = append(r.Revisions, Revision{Sequence: seq, Action: action, Note: note, Columns: cloneColumns(columns)})
}

// Normalize validates and fills a row offered for storage under the current
// schema. An omitted column uses its default when one is defined; an explicit
// null is never replaced by a default.
func (r Relation) Normalize(row datum.Row) (datum.Row, error) {
	for k := range row {
		if _, ok := r.Column(k); !ok {
			return nil, fmt.Errorf("unknown column %q", k)
		}
	}
	out := make(datum.Row, len(r.Columns))
	for _, c := range r.Columns {
		v, exists := row[c.Name]
		if !exists {
			if c.Default != nil && !c.Default.Null() {
				v = *c.Default
			} else {
				if !c.Nullable {
					return nil, fmt.Errorf("column %s cannot be null", c.Name)
				}
				v = datum.Value{Kind: "null"}
			}
		} else if v.Null() && !c.Nullable {
			return nil, fmt.Errorf("column %s cannot be null", c.Name)
		}
		if !v.Null() && v.Kind != c.Type {
			return nil, fmt.Errorf("column %s requires %s", c.Name, c.Type)
		}
		if v.Kind == "text" && len(v.Text) > 65536 {
			return nil, fmt.Errorf("text exceeds 64KiB")
		}
		out[c.Name] = v
	}
	return out, nil
}

// Interpret projects a row written under an older revision onto this schema.
// Missing columns are filled from the revision default (or null when
// nullable); stored values, including explicit nulls, are never rewritten.
func (r Relation) Interpret(row datum.Row) (datum.Row, error) {
	out := make(datum.Row, len(r.Columns))
	for _, c := range r.Columns {
		v, exists := row[c.Name]
		if !exists {
			if c.Default != nil && !c.Default.Null() {
				v = *c.Default
			} else {
				if !c.Nullable {
					return nil, fmt.Errorf("historical row has no value for non-null column %s", c.Name)
				}
				v = datum.Value{Kind: "null"}
			}
		}
		if !v.Null() && v.Kind != c.Type {
			return nil, fmt.Errorf("column %s requires %s", c.Name, c.Type)
		}
		if v.Kind == "text" && len(v.Text) > 65536 {
			return nil, fmt.Errorf("text exceeds 64KiB")
		}
		out[c.Name] = v
	}
	return out, nil
}

func (r Relation) Key(row datum.Row) string { return row[r.Primary].Key() }

func cloneColumns(in []Column) []Column {
	out := make([]Column, len(in))
	for i, c := range in {
		out[i] = c
		if c.Default != nil {
			v := *c.Default
			out[i].Default = &v
		}
	}
	return out
}
