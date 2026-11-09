package schema

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

func loadDump(t *testing.T) *Catalog {
	t.Helper()
	f, err := os.Open("testdata/dump.sql")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cat, err := ParseDDL(f)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func TestParseDumpOrders(t *testing.T) {
	cat := loadDump(t)
	orders, err := cat.Lookup("orders")
	if err != nil {
		t.Fatal(err)
	}
	if orders.Unsupported != "" {
		t.Fatalf("orders marked unsupported: %s", orders.Unsupported)
	}

	type want struct {
		name       string
		kind       TypeKind
		typeName   string
		notNull    bool
		hasDefault bool
		generated  bool
	}
	wants := []want{
		{"id", TypeInt8, "bigint", true, true, false},
		{"customer_id", TypeInt4, "integer", true, false, false},
		{"status", TypeEnum, "order_status", true, true, false},
		{"amount", TypeNumeric, "numeric(12,2)", true, false, false},
		{"currency", TypeBpchar, "character(3)", true, true, false},
		{"email", TypeVarchar, "character varying(64)", false, true, false},
		{"Note", TypeText, "text", false, false, false},
		{"paid", TypeBool, "boolean", true, true, false},
		{"created_at", TypeTimestampTZ, "timestamp(3) with time zone", true, true, false},
		{"shipped_on", TypeDate, "date", false, false, false},
		{"external_id", TypeUUID, "uuid", false, false, false},
		{"tags", TypeUnknown, "text[]", false, false, false},
		{"char", TypeUnknown, `"char"`, false, false, false},
		{"meta", TypeJSONB, "jsonb", false, false, false},
		{"total", TypeNumeric, "numeric(14,2)", false, false, true},
	}
	if len(orders.Columns) != len(wants) {
		t.Fatalf("got %d columns, want %d", len(orders.Columns), len(wants))
	}
	for i, w := range wants {
		c := orders.Columns[i]
		got := want{c.Name, c.Type.Kind, c.Type.Name, c.NotNull, c.HasDefault, c.Generated}
		if got != w {
			t.Errorf("column %d = %+v, want %+v", i, got, w)
		}
	}
	status, _ := orders.Column("status")
	if !slices.Equal(status.Type.Labels, []string{"new", "paid", "shipped", "cancelled"}) {
		t.Errorf("enum labels = %q", status.Type.Labels)
	}
	amount, _ := orders.Column("amount")
	if amount.Type.Precision != 12 || amount.Type.Scale != 2 {
		t.Errorf("amount typmod = (%d,%d)", amount.Type.Precision, amount.Type.Scale)
	}

	// The expression index and the partial index cannot be checked from the
	// file, so only the two constraints remain.
	var keys []string
	for _, k := range orders.Keys {
		keys = append(keys, k.Kind.String()+" "+k.Name+"("+strings.Join(k.Columns, ",")+")")
	}
	wantKeys := []string{"PRIMARY KEY orders_pkey(id)", "UNIQUE orders_external_id_key(external_id)"}
	if !slices.Equal(keys, wantKeys) {
		t.Errorf("keys = %q, want %q", keys, wantKeys)
	}
}

func TestParseDumpOtherTables(t *testing.T) {
	cat := loadDump(t)

	inv, err := cat.Lookup("invoices")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Name.String() != "billing.invoices" {
		t.Errorf("name = %s", inv.Name)
	}
	id, _ := inv.Column("id")
	if !id.HasDefault || !id.NotNull {
		t.Errorf("identity column: HasDefault=%v NotNull=%v", id.HasDefault, id.NotNull)
	}
	if len(inv.Keys) != 1 || !inv.Keys[0].NullsNotDistinct || inv.Keys[0].Name != "invoices_order_idx" {
		t.Errorf("keys = %+v", inv.Keys)
	}

	archive, err := cat.Lookup("public.orders_archive")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(archive.Unsupported, "INHERITS") {
		t.Errorf("Unsupported = %q", archive.Unsupported)
	}
}

func TestLookup(t *testing.T) {
	cat := loadDump(t)
	tests := []struct {
		name string
		err  error
	}{
		{"public.orders", nil},
		{`"orders"`, nil},
		{"billing.orders", ErrTableNotFound},
		{"nope", ErrTableNotFound},
		{"", ErrTableAmbiguous},
	}
	for _, tt := range tests {
		_, err := cat.Lookup(tt.name)
		if !errors.Is(err, tt.err) {
			t.Errorf("Lookup(%q) err = %v, want %v", tt.name, err, tt.err)
		}
	}
}

func TestParseHandWritten(t *testing.T) {
	ddl := `
create table Events (
  id serial primary key,
  kind varchar not null,
  code char,
  ratio float(10),
  score float,
  price decimal(5),
  happened timestamptz,
  email text unique nulls not distinct,
  unique (kind, code)
);`
	cat, err := ParseDDL(strings.NewReader(ddl))
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := cat.Lookup("")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range tbl.Columns {
		got[c.Name] = c.Type.Name
	}
	want := map[string]string{
		"id": "integer", "kind": "character varying", "code": "character(1)", "ratio": "real",
		"score": "double precision", "price": "numeric(5,0)", "happened": "timestamp with time zone", "email": "text",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s type = %q, want %q", k, got[k], v)
		}
	}
	id, _ := tbl.Column("id")
	if !id.HasDefault || !id.NotNull {
		t.Errorf("serial: HasDefault=%v NotNull=%v", id.HasDefault, id.NotNull)
	}
	if len(tbl.Keys) != 3 || tbl.Keys[0].Name != "events_pkey" || !tbl.Keys[1].NullsNotDistinct ||
		!slices.Equal(tbl.Keys[2].Columns, []string{"kind", "code"}) {
		t.Errorf("keys = %+v", tbl.Keys)
	}
}

func TestLexErrors(t *testing.T) {
	for _, src := range []string{"select 'open", `create table "x (`, "/* never closed", "select $tag$ body"} {
		_, err := ParseDDL(strings.NewReader(src))
		var se *SyntaxError
		if !errors.As(err, &se) {
			t.Errorf("ParseDDL(%q) err = %v, want *SyntaxError", src, err)
		}
	}
}
