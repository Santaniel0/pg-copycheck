package scan

import (
	"fmt"
	"slices"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
	"github.com/Santaniel0/pg-copycheck/internal/pginput"
	"github.com/Santaniel0/pg-copycheck/internal/schema"
)

// SetupError is an error COPY raises before reading any data, such as an
// unknown column in the column list.
type SetupError struct {
	Message string
	Detail  string
}

func (e *SetupError) Error() string {
	if e.Detail != "" {
		return e.Message + " (" + e.Detail + ")"
	}
	return e.Message
}

// plan is the column mapping COPY would use, with a checker per column.
type plan struct {
	rel          string
	cols         []*schema.Column
	checkers     []pginput.Checker
	notNull      []bool
	forceNull    []bool
	forceNotNull []bool
	nullStr      []byte
	keys         []keyPlan
	setup        []Problem
	keyStatus    []KeyStatus
}

type keyPlan struct {
	name             string
	cols             []int
	canon            []pginput.Canonicalizer
	nullsNotDistinct bool
	status           int // index into plan.keyStatus
}

func notNullProblem(col, rel string) Problem {
	msg := fmt.Sprintf("null value in column %q of relation %q violates not-null constraint", col, rel)
	return Problem{Severity: SeverityError, SQLState: "23502", Title: msg, Column: col, Message: msg}
}

// newPlan resolves the column list the way CopyGetAttnums does and attaches
// the checkers.
func newPlan(t *schema.Table, names []string, opts copyopt.Options, checkUnique bool) (*plan, error) {
	if t.Unsupported != "" {
		return nil, &SetupError{Message: fmt.Sprintf("table %s cannot be checked: %s", t.Name, t.Unsupported)}
	}
	p := &plan{rel: t.Name.Name, nullStr: []byte(opts.Null)}
	if len(names) == 0 {
		for _, c := range t.Columns {
			if !c.Generated {
				p.cols = append(p.cols, c)
			}
		}
	} else {
		for _, n := range names {
			c, ok := t.Column(n)
			if !ok {
				return nil, &SetupError{Message: fmt.Sprintf("column %q of relation %q does not exist", n, p.rel)}
			}
			if slices.Contains(p.cols, c) {
				return nil, &SetupError{Message: fmt.Sprintf("column %q specified more than once", n)}
			}
			if c.Generated {
				return nil, &SetupError{
					Message: fmt.Sprintf("column %q is a generated column", n),
					Detail:  "Generated columns cannot be used in COPY.",
				}
			}
			p.cols = append(p.cols, c)
		}
	}

	var err error
	if p.forceNull, err = p.flags(opts.ForceNull, "FORCE_NULL"); err != nil {
		return nil, err
	}
	if p.forceNotNull, err = p.flags(opts.ForceNotNull, "FORCE_NOT_NULL"); err != nil {
		return nil, err
	}

	settings := pginput.Settings{Version: opts.Version, DateOrder: opts.DateOrder}
	for _, c := range p.cols {
		p.checkers = append(p.checkers, pginput.For(c.Type, settings))
		p.notNull = append(p.notNull, c.NotNull)
	}

	// Columns left out of the list get their default; without one a NOT NULL
	// column fails every row.
	for _, c := range t.Columns {
		if !slices.Contains(p.cols, c) && c.NotNull && !c.HasDefault && !c.Generated {
			p.setup = append(p.setup, notNullProblem(c.Name, p.rel))
		}
	}

	if checkUnique {
		p.planKeys(t)
	}
	return p, nil
}

func (p *plan) flags(set copyopt.ColumnSet, option string) ([]bool, error) {
	flags := make([]bool, len(p.cols))
	if set.All {
		for i := range flags {
			flags[i] = true
		}
		return flags, nil
	}
	for _, n := range set.Names {
		i := slices.IndexFunc(p.cols, func(c *schema.Column) bool { return c.Name == n })
		if i < 0 {
			return nil, &SetupError{Message: fmt.Sprintf("%s column %q not referenced by COPY", option, n)}
		}
		flags[i] = true
	}
	return flags, nil
}

func (p *plan) planKeys(t *schema.Table) {
	for _, k := range t.Keys {
		st := KeyStatus{Name: k.Name, Columns: k.Columns}
		kp := keyPlan{name: k.Name, nullsNotDistinct: k.NullsNotDistinct}
		for _, name := range k.Columns {
			i := slices.IndexFunc(p.cols, func(c *schema.Column) bool { return c.Name == name })
			if i < 0 {
				st.Reason = fmt.Sprintf("column %q is not loaded from the file", name)
				break
			}
			canon, ok := p.checkers[i].(pginput.Canonicalizer)
			if !ok {
				st.Reason = fmt.Sprintf("values of type %s cannot be compared offline", p.cols[i].Type.Name)
				break
			}
			kp.cols = append(kp.cols, i)
			kp.canon = append(kp.canon, canon)
		}
		st.Checked = st.Reason == ""
		kp.status = len(p.keyStatus)
		p.keyStatus = append(p.keyStatus, st)
		if st.Checked {
			p.keys = append(p.keys, kp)
		}
	}
}
