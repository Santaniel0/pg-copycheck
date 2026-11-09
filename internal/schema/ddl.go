package schema

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ParseDDL reads CREATE TABLE, CREATE TYPE ... AS ENUM, ALTER TABLE ... ADD
// PRIMARY KEY / UNIQUE, column DEFAULT / NOT NULL changes and CREATE UNIQUE
// INDEX on plain columns. Every other statement is skipped. Constructs that
// would make a table's column list unknowable (INHERITS, PARTITION OF, LIKE,
// OF type) mark that table as unsupported instead of failing the whole file.
func ParseDDL(r io.Reader) (*Catalog, error) {
	src, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read DDL: %w", err)
	}
	toks, err := lex(string(src))
	if err != nil {
		return nil, err
	}
	p := &parser{cat: &Catalog{enums: map[QualifiedName][]string{}}}
	start := 0
	for i, t := range toks {
		if t.is(";") {
			p.statement(toks[start:i])
			start = i + 1
		}
	}
	p.statement(toks[start:])
	p.resolveEnums()
	return p.cat, nil
}

type pendingType struct {
	col *Column
	ref QualifiedName
}

type parser struct {
	cat     *Catalog
	pending []pendingType
}

func words(toks []token, i int, want ...string) bool {
	if i+len(want) > len(toks) {
		return false
	}
	for k, w := range want {
		if toks[i+k].word() != w {
			return false
		}
	}
	return true
}

func parseQualifiedName(toks []token, i int) (QualifiedName, int, bool) {
	if i >= len(toks) || !toks[i].isName() {
		return QualifiedName{}, i, false
	}
	first := toks[i].text
	if i+2 < len(toks) && toks[i+1].is(".") && toks[i+2].isName() {
		return QualifiedName{Schema: first, Name: toks[i+2].text}, i + 3, true
	}
	return QualifiedName{Name: first}, i + 1, true
}

// splitTopLevel splits toks on commas outside parentheses.
func splitTopLevel(toks []token) [][]token {
	var parts [][]token
	depth, start := 0, 0
	for i, t := range toks {
		switch {
		case t.is("(") || t.is("["):
			depth++
		case t.is(")") || t.is("]"):
			depth--
		case t.is(",") && depth == 0:
			parts = append(parts, toks[start:i])
			start = i + 1
		}
	}
	return append(parts, toks[start:])
}

// group returns the index just past the parenthesised group opening at i.
func group(toks []token, i int) int {
	depth := 0
	for ; i < len(toks); i++ {
		switch {
		case toks[i].is("("):
			depth++
		case toks[i].is(")"):
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return i
}

func (p *parser) statement(toks []token) {
	switch {
	case words(toks, 0, "create"):
		p.create(toks[1:])
	case words(toks, 0, "alter", "table"):
		p.alterTable(toks[2:])
	}
}

func (p *parser) create(toks []token) {
	i := 0
	for i < len(toks) && (toks[i].word() == "global" || toks[i].word() == "local" ||
		toks[i].word() == "temporary" || toks[i].word() == "temp" || toks[i].word() == "unlogged") {
		i++
	}
	switch {
	case words(toks, i, "table"):
		p.createTable(toks[i+1:])
	case words(toks, i, "type"):
		p.createEnum(toks[i+1:])
	case words(toks, i, "unique", "index"):
		p.createUniqueIndex(toks[i+2:])
	}
}

func (p *parser) createEnum(toks []token) {
	name, i, ok := parseQualifiedName(toks, 0)
	if !ok || !words(toks, i, "as", "enum") || i+2 >= len(toks) || !toks[i+2].is("(") {
		return
	}
	var labels []string
	for _, t := range toks[i+3:] {
		if t.kind == tokString {
			labels = append(labels, t.text)
		}
	}
	if name.Schema == "" {
		name.Schema = "public"
	}
	p.cat.enums[name] = labels
}

func (p *parser) createTable(toks []token) {
	i := 0
	if words(toks, i, "if", "not", "exists") {
		i += 3
	}
	name, i, ok := parseQualifiedName(toks, i)
	if !ok {
		return
	}
	if name.Schema == "" {
		name.Schema = "public"
	}
	t := &Table{Name: name}
	p.cat.tables = append(p.cat.tables, t)

	switch {
	case words(toks, i, "partition", "of"):
		t.Unsupported = "it is a partition (PARTITION OF); check against the parent table instead"
		return
	case words(toks, i, "of"):
		t.Unsupported = "it is a typed table (OF type)"
		return
	case i >= len(toks) || !toks[i].is("("):
		t.Unsupported = "its column list could not be read"
		return
	}
	end := group(toks, i)
	for _, el := range splitTopLevel(toks[i+1 : end-1]) {
		if len(el) > 0 {
			p.tableElement(t, el)
		}
	}
	for j := end; j < len(toks); j++ {
		if toks[j].word() == "inherits" {
			t.Unsupported = "it inherits columns from another table (INHERITS)"
		}
	}
}

func (p *parser) tableElement(t *Table, el []token) {
	switch el[0].word() {
	case "constraint", "primary", "unique", "check", "foreign", "exclude":
		p.tableConstraint(t, el)
	case "like":
		t.Unsupported = "it copies columns from another table (LIKE)"
	default:
		p.column(t, el)
	}
}

func isColumnStop(toks []token, i int) bool {
	switch toks[i].word() {
	case "null", "default", "constraint", "primary", "unique", "check", "references",
		"generated", "collate", "compression", "deferrable", "initially":
		return true
	case "not":
		return i+1 < len(toks) && (toks[i+1].word() == "null" || toks[i+1].word() == "deferrable")
	}
	return false
}

func (p *parser) column(t *Table, el []token) {
	if !el[0].isName() {
		return
	}
	col := &Column{Name: el[0].text}
	t.Columns = append(t.Columns, col)

	i, depth := 1, 0
	for ; i < len(el); i++ {
		if el[i].is("(") {
			depth++
		} else if el[i].is(")") {
			depth--
		}
		if depth == 0 && isColumnStop(el, i) {
			break
		}
	}
	typ, serial, ref, isRef := parseType(el[1:i])
	col.Type = typ
	if serial {
		col.NotNull, col.HasDefault = true, true
	}
	if isRef {
		p.pending = append(p.pending, pendingType{col, ref})
	}

	for i < len(el) {
		switch w := el[i].word(); {
		case w == "constraint":
			i += 2
		case w == "not" && words(el, i, "not", "null"):
			col.NotNull = true
			i += 2
		case w == "default":
			col.HasDefault = true
			i = skipExpr(el, i+1)
		case w == "primary":
			t.Keys = append(t.Keys, Key{Name: t.Name.Name + "_pkey", Kind: KeyPrimary, Columns: []string{col.Name}})
			col.NotNull = true
			i += 2
		case w == "unique":
			k := Key{Name: t.Name.Name + "_" + col.Name + "_key", Kind: KeyUnique, Columns: []string{col.Name}}
			i++
			if words(el, i, "nulls", "not", "distinct") {
				k.NullsNotDistinct = true
				i += 3
			}
			t.Keys = append(t.Keys, k)
		case w == "generated":
			identity := false
			j := i + 1
			for ; j < len(el) && !el[j].is("("); j++ {
				if el[j].word() == "identity" {
					identity = true
				}
			}
			if identity {
				col.HasDefault = true
			} else {
				col.Generated = true
			}
			i = skipExpr(el, i+1)
		case w == "check":
			i = group(el, i+1)
		default:
			// NULL, REFERENCES ..., COLLATE ..., DEFERRABLE and friends.
			i = skipTail(el, i+1)
		}
	}
}

// skipExpr skips at least one token or group starting at i (DEFAULT NULL
// must not stop at its own NULL), then everything up to the next column
// constraint keyword.
func skipExpr(el []token, i int) int {
	if i < len(el) && el[i].is("(") {
		i = group(el, i)
	} else {
		i++
	}
	return skipTail(el, i)
}

func skipTail(el []token, i int) int {
	for i < len(el) && !isColumnStop(el, i) {
		if el[i].is("(") {
			i = group(el, i)
			continue
		}
		i++
	}
	return i
}

func (p *parser) tableConstraint(t *Table, el []token) {
	i := 0
	name := ""
	if el[0].word() == "constraint" && len(el) > 1 {
		name = el[1].text
		i = 2
	}
	var k Key
	switch {
	case words(el, i, "primary", "key"):
		k = Key{Name: name, Kind: KeyPrimary}
		if name == "" {
			k.Name = t.Name.Name + "_pkey"
		}
		i += 2
	case words(el, i, "unique"):
		k = Key{Name: name, Kind: KeyUnique}
		i++
		if words(el, i, "nulls", "not", "distinct") {
			k.NullsNotDistinct = true
			i += 3
		}
	default:
		return
	}
	cols, ok := columnList(el, i)
	if !ok {
		return
	}
	k.Columns = cols
	if k.Name == "" {
		k.Name = t.Name.Name + "_" + strings.Join(cols, "_") + "_key"
	}
	p.addKey(t, k)
}

func (p *parser) addKey(t *Table, k Key) {
	if k.Kind == KeyPrimary {
		for _, name := range k.Columns {
			if c, ok := t.Column(name); ok {
				c.NotNull = true
			}
		}
	}
	t.Keys = append(t.Keys, k)
}

// columnList reads "(a, b)" at i. It fails when any element is more than a
// plain column name with an optional sort order.
func columnList(toks []token, i int) ([]string, bool) {
	if i >= len(toks) || !toks[i].is("(") {
		return nil, false
	}
	end := group(toks, i)
	var cols []string
	for _, part := range splitTopLevel(toks[i+1 : end-1]) {
		if len(part) == 0 || !part[0].isName() {
			return nil, false
		}
		for _, t := range part[1:] {
			switch t.word() {
			case "asc", "desc", "nulls", "first", "last":
			default:
				return nil, false
			}
		}
		cols = append(cols, part[0].text)
	}
	return cols, true
}

func (p *parser) table(q QualifiedName) *Table {
	if q.Schema == "" {
		q.Schema = "public"
	}
	return p.cat.find(q)
}

func (p *parser) alterTable(toks []token) {
	i := 0
	if words(toks, i, "if", "exists") {
		i += 2
	}
	if words(toks, i, "only") {
		i++
	}
	name, i, ok := parseQualifiedName(toks, i)
	if !ok {
		return
	}
	t := p.table(name)
	if t == nil {
		return
	}
	for _, act := range splitTopLevel(toks[i:]) {
		switch {
		case words(act, 0, "add") && len(act) > 1:
			switch act[1].word() {
			case "constraint", "primary", "unique", "check", "foreign", "exclude":
				p.tableConstraint(t, act[1:])
			case "column":
				if len(act) > 2 {
					p.column(t, act[2:])
				}
			default:
				p.column(t, act[1:])
			}
		case words(act, 0, "alter"):
			p.alterColumn(t, act[1:])
		}
	}
}

func (p *parser) alterColumn(t *Table, act []token) {
	j := 0
	if words(act, j, "column") {
		j++
	}
	if j >= len(act) || !act[j].isName() {
		return
	}
	col, ok := t.Column(act[j].text)
	if !ok {
		return
	}
	j++
	switch {
	case words(act, j, "set", "default"), words(act, j, "add", "generated"):
		col.HasDefault = true
	case words(act, j, "drop", "default"):
		col.HasDefault = false
	case words(act, j, "set", "not", "null"):
		col.NotNull = true
	case words(act, j, "drop", "not", "null"):
		col.NotNull = false
	}
}

func (p *parser) createUniqueIndex(toks []token) {
	i := 0
	for words(toks, i, "concurrently") || words(toks, i, "if", "not", "exists") {
		if toks[i].word() == "if" {
			i += 3
		} else {
			i++
		}
	}
	name := ""
	if i < len(toks) && toks[i].word() != "on" {
		name = toks[i].text
		i++
	}
	if !words(toks, i, "on") {
		return
	}
	i++
	if words(toks, i, "only") {
		i++
	}
	tname, i, ok := parseQualifiedName(toks, i)
	if !ok {
		return
	}
	t := p.table(tname)
	if t == nil {
		return
	}
	if words(toks, i, "using") {
		i += 2
	}
	cols, ok := columnList(toks, i)
	if !ok {
		return // expression index or operator class: not checked
	}
	k := Key{Name: name, Kind: KeyUnique, Columns: cols}
	for j := group(toks, i); j < len(toks); j++ {
		switch {
		case toks[j].word() == "where":
			return // partial index
		case words(toks, j, "nulls", "not", "distinct"):
			k.NullsNotDistinct = true
		}
	}
	if k.Name == "" {
		k.Name = t.Name.Name + "_" + strings.Join(cols, "_") + "_idx"
	}
	t.Keys = append(t.Keys, k)
}

func (p *parser) resolveEnums() {
	for _, pt := range p.pending {
		ref := pt.ref
		if ref.Schema == "" {
			ref.Schema = "public"
		}
		labels, ok := p.cat.enums[ref]
		if !ok {
			continue
		}
		display := ref.Name
		if ref.Schema != "public" {
			display = ref.String()
		}
		pt.col.Type = Type{Kind: TypeEnum, Name: display, Precision: NoTypmod, Length: NoTypmod, Labels: labels}
	}
}

// parseType maps the tokens of a column type to a Type. It also reports
// serial pseudo-types and, for names that may refer to a user-defined type,
// the name to resolve later.
func parseType(toks []token) (typ Type, serial bool, ref QualifiedName, isRef bool) {
	var parts []string
	var mods []int
	array, depth, neg := false, 0, false
	for _, t := range toks {
		switch {
		case t.is("("):
			depth++
		case t.is(")"):
			depth--
		case t.is("[") || t.is("]") || t.word() == "array":
			array = true
		case depth > 0 && t.is("-"):
			neg = true
		case depth > 0 && t.kind == tokNumber:
			n, _ := strconv.Atoi(t.text)
			if neg {
				n = -n
			}
			mods, neg = append(mods, n), false
		case depth > 0:
		case t.kind == tokIdent:
			parts = append(parts, t.text)
		case t.kind == tokQuoted:
			parts = append(parts, `"`+t.text+`"`)
		case t.is("."):
			parts = append(parts, ".")
		}
	}
	name := strings.ReplaceAll(strings.Join(parts, " "), " . ", ".")
	name = strings.TrimPrefix(name, "pg_catalog.")
	typ = Type{Kind: TypeUnknown, Name: name, Precision: NoTypmod, Length: NoTypmod}
	if array {
		typ.Name += "[]"
		return typ, false, ref, false
	}
	mod := func(k int) int {
		if k < len(mods) {
			return mods[k]
		}
		return NoTypmod
	}
	switch name {
	case "smallint", "int2":
		typ.Kind = TypeInt2
	case "smallserial", "serial2":
		typ.Kind, serial = TypeInt2, true
	case "integer", "int", "int4":
		typ.Kind = TypeInt4
	case "serial", "serial4":
		typ.Kind, serial = TypeInt4, true
	case "bigint", "int8":
		typ.Kind = TypeInt8
	case "bigserial", "serial8":
		typ.Kind, serial = TypeInt8, true
	case "numeric", "decimal":
		typ.Kind = TypeNumeric
		if len(mods) > 0 {
			typ.Precision, typ.Scale = mods[0], 0
			if len(mods) > 1 {
				typ.Scale = mods[1]
			}
		}
	case "real", "float4":
		typ.Kind = TypeFloat4
	case "double precision", "float8":
		typ.Kind = TypeFloat8
	case "float":
		typ.Kind = TypeFloat8
		if p := mod(0); p != NoTypmod && p <= 24 {
			typ.Kind = TypeFloat4
		}
	case "boolean", "bool":
		typ.Kind = TypeBool
	case "text":
		typ.Kind = TypeText
	case "character varying", "varchar":
		typ.Kind, typ.Length = TypeVarchar, mod(0)
	case "character", "char":
		typ.Kind, typ.Length = TypeBpchar, 1
		if l := mod(0); l != NoTypmod {
			typ.Length = l
		}
	case "bpchar":
		typ.Kind, typ.Length = TypeBpchar, mod(0)
	case "date":
		typ.Kind = TypeDate
	case "timestamp", "timestamp without time zone":
		typ.Kind, typ.Precision = TypeTimestamp, mod(0)
	case "timestamp with time zone", "timestamptz":
		typ.Kind, typ.Precision = TypeTimestampTZ, mod(0)
	case "uuid":
		typ.Kind = TypeUUID
	case "json":
		typ.Kind = TypeJSON
	case "jsonb":
		typ.Kind = TypeJSONB
	default:
		if q, n, ok := parseQualifiedName(toks, 0); ok && n == len(toks) {
			return typ, false, q, true
		}
		return typ, false, ref, false
	}
	typ.Name = canonicalTypeName(typ)
	return typ, serial, ref, false
}

func canonicalTypeName(t Type) string {
	base := t.Kind.String()
	switch t.Kind {
	case TypeNumeric:
		if t.Precision != NoTypmod {
			return fmt.Sprintf("numeric(%d,%d)", t.Precision, t.Scale)
		}
	case TypeVarchar, TypeBpchar:
		if t.Length != NoTypmod {
			return fmt.Sprintf("%s(%d)", base, t.Length)
		}
		if t.Kind == TypeBpchar {
			return "bpchar"
		}
	case TypeTimestamp, TypeTimestampTZ:
		if t.Precision != NoTypmod {
			return strings.Replace(base, "timestamp", fmt.Sprintf("timestamp(%d)", t.Precision), 1)
		}
	}
	return base
}
