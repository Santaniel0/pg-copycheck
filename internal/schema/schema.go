// Package schema reads table definitions from the SQL that
// pg_dump --schema-only produces.
package schema

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// TypeKind identifies the column types the checker understands. Everything
// else is TypeUnknown and its values are reported as unchecked.
type TypeKind uint8

const (
	TypeUnknown TypeKind = iota
	TypeInt2
	TypeInt4
	TypeInt8
	TypeNumeric
	TypeFloat4
	TypeFloat8
	TypeBool
	TypeText
	TypeVarchar
	TypeBpchar
	TypeDate
	TypeTimestamp
	TypeTimestampTZ
	TypeUUID
	TypeJSON
	TypeJSONB
	TypeEnum
)

var kindNames = [...]string{
	TypeUnknown:     "unknown",
	TypeInt2:        "smallint",
	TypeInt4:        "integer",
	TypeInt8:        "bigint",
	TypeNumeric:     "numeric",
	TypeFloat4:      "real",
	TypeFloat8:      "double precision",
	TypeBool:        "boolean",
	TypeText:        "text",
	TypeVarchar:     "character varying",
	TypeBpchar:      "character",
	TypeDate:        "date",
	TypeTimestamp:   "timestamp without time zone",
	TypeTimestampTZ: "timestamp with time zone",
	TypeUUID:        "uuid",
	TypeJSON:        "json",
	TypeJSONB:       "jsonb",
	TypeEnum:        "enum",
}

func (k TypeKind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "TypeKind(" + strconv.Itoa(int(k)) + ")"
}

// NoTypmod marks a type declared without a modifier, such as plain numeric
// or varchar.
const NoTypmod = -1

// Type is a column type with its modifier.
type Type struct {
	Kind TypeKind
	// Name is the type as written in the DDL, normalised to lower case. For
	// enums it is the name PostgreSQL prints in error messages.
	Name string
	// Precision and Scale are set for numeric(p,s); Precision is NoTypmod
	// otherwise. For timestamps Precision holds the fractional digits.
	Precision int
	Scale     int
	// Length is the character limit of varchar(n) and char(n), or NoTypmod.
	Length int
	Labels []string // enum labels in declaration order
}

func (t Type) String() string { return t.Name }

// QualifiedName is a schema-qualified object name.
type QualifiedName struct {
	Schema string
	Name   string
}

func (q QualifiedName) String() string {
	if q.Schema == "" {
		return quoteIdent(q.Name)
	}
	return quoteIdent(q.Schema) + "." + quoteIdent(q.Name)
}

func quoteIdent(s string) string {
	plain := s != ""
	for i, c := range s {
		if !(c == '_' || c >= 'a' && c <= 'z' || i > 0 && (c >= '0' && c <= '9' || c == '$')) {
			plain = false
			break
		}
	}
	if plain {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// Column is one table column.
type Column struct {
	Name    string
	Type    Type
	NotNull bool
	// HasDefault covers DEFAULT, identity and serial columns: when such a
	// column is left out of the COPY column list it still gets a value.
	HasDefault bool
	// Generated columns are skipped when no column list is given and are an
	// error when listed explicitly.
	Generated bool
}

// KeyKind distinguishes primary keys from unique constraints and indexes.
type KeyKind uint8

const (
	KeyPrimary KeyKind = iota
	KeyUnique
)

func (k KeyKind) String() string {
	if k == KeyPrimary {
		return "PRIMARY KEY"
	}
	return "UNIQUE"
}

// Key is a primary key, unique constraint or plain-column unique index.
type Key struct {
	Name             string
	Kind             KeyKind
	Columns          []string
	NullsNotDistinct bool
}

// Table is a table definition.
type Table struct {
	Name    QualifiedName
	Columns []*Column
	Keys    []Key
	// Unsupported explains why the table cannot be checked, for example
	// because it inherits columns from a parent the DDL does not describe.
	Unsupported string
}

// Column returns the column with the exact given name.
func (t *Table) Column(name string) (*Column, bool) {
	for _, c := range t.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return nil, false
}

// Catalog holds what was read from a DDL file.
type Catalog struct {
	tables []*Table
	enums  map[QualifiedName][]string
}

// Tables returns the tables in the order they were defined.
func (c *Catalog) Tables() []*Table { return c.tables }

// Sentinel errors for table lookup.
var (
	ErrTableNotFound  = errors.New("table not found in DDL")
	ErrTableAmbiguous = errors.New("table name is ambiguous")
)

// Lookup finds a table by "name" or "schema.name". An unqualified name is
// resolved the way the default search_path would: public first. An empty
// name selects the only table in the catalog.
func (c *Catalog) Lookup(name string) (*Table, error) {
	if name == "" {
		switch len(c.tables) {
		case 0:
			return nil, fmt.Errorf("%w: the DDL defines no tables", ErrTableNotFound)
		case 1:
			return c.tables[0], nil
		}
		names := make([]string, len(c.tables))
		for i, t := range c.tables {
			names[i] = t.Name.String()
		}
		return nil, fmt.Errorf("%w: the DDL defines %d tables (%s); choose one", ErrTableAmbiguous, len(c.tables), strings.Join(names, ", "))
	}
	toks, err := lex(name)
	if err != nil {
		return nil, err
	}
	q, n, ok := parseQualifiedName(toks, 0)
	if !ok || n != len(toks) {
		return nil, fmt.Errorf("invalid table name %q", name)
	}
	if q.Schema == "" {
		q.Schema = "public"
		if t := c.find(q); t != nil {
			return t, nil
		}
		var found []*Table
		for _, t := range c.tables {
			if t.Name.Name == q.Name {
				found = append(found, t)
			}
		}
		switch len(found) {
		case 0:
			return nil, fmt.Errorf("%w: %s", ErrTableNotFound, name)
		case 1:
			return found[0], nil
		}
		return nil, fmt.Errorf("%w: %q exists in several schemas; qualify it", ErrTableAmbiguous, name)
	}
	if t := c.find(q); t != nil {
		return t, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrTableNotFound, q)
}

func (c *Catalog) find(q QualifiedName) *Table {
	for _, t := range c.tables {
		if t.Name == q {
			return t
		}
	}
	return nil
}
