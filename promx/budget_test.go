package promx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/baditaflorin/go-common/testhelpers/promxtest"
	"github.com/prometheus/client_golang/prometheus"
)

// budgetRow is one row of the "Series budgets" table in docs/observability.md.
type budgetRow struct {
	metric string // a name, or a prefix ending in * that stands for every metric starting with it
	labels []string
	budget int
}

func (r budgetRow) matches(name string) bool {
	if p, ok := strings.CutSuffix(r.metric, "*"); ok {
		return strings.HasPrefix(name, p)
	}
	return r.metric == name
}

// loadBudgets reads the table between "## Series budgets" and the next "## ". Each
// row is | `metric` | labels besides service, comma separated, or - | budget | why |.
func loadBudgets(t testing.TB) []budgetRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "docs", "observability.md"))
	if err != nil {
		t.Fatalf("the series budgets live in docs/observability.md: %v", err)
	}
	_, section, ok := strings.Cut(string(raw), "\n## Series budgets\n")
	if !ok {
		t.Fatalf("docs/observability.md has no \"## Series budgets\" section")
	}
	if i := strings.Index(section, "\n## "); i >= 0 {
		section = section[:i]
	}
	var rows []budgetRow
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "| "), "|")
		if len(cells) < 3 {
			t.Fatalf("budget row needs a metric, labels and a budget: %q", line)
		}
		n, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(cells[2]), ",", ""))
		if err != nil {
			t.Fatalf("budget row %q: budget is not a number: %v", line, err)
		}
		row := budgetRow{metric: strings.Trim(strings.TrimSpace(cells[0]), "`"), budget: n}
		for _, l := range strings.Split(cells[1], ",") {
			if l = strings.Trim(strings.TrimSpace(l), "`"); l != "" && l != "-" {
				row.labels = append(row.labels, l)
			}
		}
		sort.Strings(row.labels)
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		t.Fatalf("the Series budgets section of docs/observability.md has no rows")
	}
	return rows
}

// lookup finds the row for a metric: an exact name wins over a prefix row.
func lookup(rows []budgetRow, name string) (int, bool) {
	best := -1
	for i, r := range rows {
		switch {
		case r.metric == name:
			return i, true
		case strings.HasSuffix(r.metric, "*") && r.matches(name) && (best < 0 || len(r.metric) > len(rows[best].metric)):
			best = i
		}
	}
	return best, best >= 0
}

func withoutService(labels []string) []string {
	var out []string
	for _, l := range labels {
		if l != "service" {
			out = append(out, l)
		}
	}
	return out
}

func suggestedRow(m promxtest.Metric) string {
	return "| `" + m.Name + "` | " + joinOr(withoutService(m.Labels()), "-") + " | " + strconv.Itoa(m.Ceiling()) + " | |"
}

func joinOr(s []string, empty string) string {
	if len(s) == 0 {
		return empty
	}
	return strings.Join(s, ", ")
}

// TestCollectorsAreBounded runs every collector set's hostile probes through
// promxtest.AssertBounded, with the budget its metrics have in the table.
func TestCollectorsAreBounded(t *testing.T) {
	rows := loadBudgets(t)
	for _, c := range harnessCases() {
		t.Run(c.name, func(t *testing.T) {
			total := 0
			for _, m := range c.metrics {
				i, ok := lookup(rows, m)
				if !ok {
					t.Fatalf("%s has no row in the Series budgets table of docs/observability.md", m)
				}
				total += rows[i].budget
			}
			c.bounded(t, total)
		})
	}
}

// TestSeriesBudgets puts every collector set in one registry, drives its probes and
// compares what each metric can cost (the product of the distinct values its labels
// took, see promxtest.Metric.Ceiling) with its row in docs/observability.md. A
// metric without a row, a row without a metric, a row whose number is not the
// ceiling, and a row whose labels are not the metric's all fail: the table is the
// one place that says what a metric costs, so it has to be exact. The numbers are a
// ceiling per process under hostile input, not what a service sees on a normal day.
func TestSeriesBudgets(t *testing.T) {
	rows := loadBudgets(t)
	reg := prometheus.NewRegistry()
	in := promxtest.NewInputs(1)
	owner := map[string]string{}
	for _, c := range harnessCases() {
		c.into(t, reg, in)
		for _, m := range c.metrics {
			if prev, dup := owner[m]; dup {
				t.Errorf("%s is claimed by the harness cases %q and %q", m, prev, c.name)
			}
			owner[m] = c.name
		}
	}
	rep := promxtest.Measure(t, reg)

	used := map[int]bool{}
	for _, m := range rep.Metrics {
		if _, ok := owner[m.Name]; !ok {
			t.Errorf("%s is registered by a harness case that does not list it in its metrics", m.Name)
		}
		i, ok := lookup(rows, m.Name)
		if !ok {
			t.Errorf("%s has no row in the Series budgets table of docs/observability.md. Add:\n%s", m.Name, suggestedRow(m))
			continue
		}
		used[i] = true
		row := rows[i]
		if got := m.Ceiling(); got != row.budget {
			t.Errorf("%s can hold %d series (labels as probed: %s), the table says %d. The table is the ceiling, neither headroom nor a guess: "+
				"change the row, or bound the label that grew", m.Name, got, labelCounts(m), row.budget)
		}
		if got, want := withoutService(m.Labels()), row.labels; !sameStrings(got, want) {
			t.Errorf("%s has labels %v besides service, the table lists %v", m.Name, got, want)
		}
	}
	for name, c := range owner {
		if _, ok := rep.Metric(name); !ok {
			t.Errorf("%s (harness case %q) has no series: its probes do not reach it", name, c)
		}
	}
	for i, r := range rows {
		if !used[i] {
			t.Errorf("the table has a row for %q that matches no metric: remove it, or fix the name", r.metric)
		}
	}
	t.Logf("%s", rep)
}

func labelCounts(m promxtest.Metric) string {
	var parts []string
	for _, l := range withoutService(m.Labels()) {
		parts = append(parts, l+"="+strconv.Itoa(m.Distinct(l)))
	}
	return joinOr(parts, "no labels besides service")
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestEveryCollectorHasAHarnessCase: a new exported New...Collectors constructor
// has to come with hostile probes and budget rows, or this fails. It reads the
// package's own source, so it cannot be satisfied by forgetting to list the new one.
func TestEveryCollectorHasAHarnessCase(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	ctor := regexp.MustCompile(`^New\w*Collectors$`)
	found := map[string]bool{"registerBuildInfo": true}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && ctor.MatchString(fn.Name.Name) {
				found[fn.Name.Name] = true
			}
		}
	}
	covered := map[string]bool{}
	for _, c := range harnessCases() {
		for _, n := range c.ctors {
			covered[n] = true
		}
	}
	for n := range found {
		if !covered[n] {
			t.Errorf("%s has no harness case: add one to harness_test.go with hostile probes for every label whose value the process does not choose", n)
		}
	}
	for n := range covered {
		if !found[n] {
			t.Errorf("a harness case covers %s, which does not exist", n)
		}
	}
}
