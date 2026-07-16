package maniflex

// The four ModelMeta accessors were linear scans of Fields/Relations, run
// several times per request — and DeleteField's scan nested inside the validate
// step's per-field loop made that pass O(n²) (MS-PERF-A). They are now backed by
// a name→position index. These tests pin the behaviour the scans had, since the
// index has to reproduce it exactly rather than merely be faster: first-wins on
// a duplicate name, a hand-built ModelMeta working without ScanModel, and an
// index that notices the relations resolveManyToMany appends after the first
// lookup has already built it.

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
)

type idxUser struct {
	BaseModel
	Email    string `json:"email"`
	Name     string `json:"name" db:"full_name"`
	TeamID   string `json:"team_id" mfx:"relation"`
	Team     *idxTeam
	Password string `json:"password" mfx:"hidden"`
}

type idxTeam struct {
	BaseModel
	Label string `json:"label"`
}

func mustScan(t *testing.T, v any) *ModelMeta {
	t.Helper()
	meta, err := ScanModel(v, ModelConfig{})
	if err != nil {
		t.Fatalf("ScanModel(%T): %v", v, err)
	}
	return meta
}

// The index must answer exactly what a scan of the same slice answers, for every
// name in the model and for names that aren't there.
func TestModelIndex_MatchesLinearScan(t *testing.T) {
	t.Parallel()

	meta := mustScan(t, &idxUser{})

	for i := range meta.Fields {
		f := &meta.Fields[i]
		if got, want := meta.FieldByDBName(f.Tags.DBName), scanFieldByDB(meta, f.Tags.DBName); got != want {
			t.Errorf("FieldByDBName(%q) = %v, scan = %v", f.Tags.DBName, got, want)
		}
		if got, want := meta.FieldByJSONName(f.Tags.JSONName), scanFieldByJSON(meta, f.Tags.JSONName); got != want {
			t.Errorf("FieldByJSONName(%q) = %v, scan = %v", f.Tags.JSONName, got, want)
		}
	}

	for _, missing := range []string{"", "nope", "Email", "id "} {
		if f := meta.FieldByDBName(missing); f != nil {
			t.Errorf("FieldByDBName(%q) = %v, want nil", missing, f)
		}
		if f := meta.FieldByJSONName(missing); f != nil {
			t.Errorf("FieldByJSONName(%q) = %v, want nil", missing, f)
		}
	}
}

// Each accessor must read its own tag: db:"full_name" renames the column but not
// the JSON name, so the two indexes cannot be one index.
func TestModelIndex_DBAndJSONNamesAreSeparate(t *testing.T) {
	t.Parallel()

	meta := mustScan(t, &idxUser{})

	if f := meta.FieldByDBName("full_name"); f == nil || f.Name != "Name" {
		t.Errorf("FieldByDBName(\"full_name\") = %v, want the Name field", f)
	}
	if f := meta.FieldByJSONName("full_name"); f != nil {
		t.Errorf("FieldByJSONName(\"full_name\") = %v, want nil (that is the DB name)", f)
	}
	if f := meta.FieldByJSONName("name"); f == nil || f.Name != "Name" {
		t.Errorf("FieldByJSONName(\"name\") = %v, want the Name field", f)
	}
}

func TestModelIndex_Relations(t *testing.T) {
	t.Parallel()

	meta := mustScan(t, &idxUser{})

	if r := meta.RelationByKey("team"); r == nil || r.RelatedModel != "idxTeam" {
		t.Errorf("RelationByKey(\"team\") = %v, want the idxTeam relation", r)
	}
	if r := meta.RelationByModel("idxTeam"); r == nil || r.RelationKey != "team" {
		t.Errorf("RelationByModel(\"idxTeam\") = %v, want the team relation", r)
	}
	if r := meta.RelationByKey("nope"); r != nil {
		t.Errorf("RelationByKey(\"nope\") = %v, want nil", r)
	}
	if r := meta.RelationByModel("nope"); r != nil {
		t.Errorf("RelationByModel(\"nope\") = %v, want nil", r)
	}
}

// The accessors return a pointer into the slice, and callers read tags through
// it. It must address the live element, not a copy.
func TestModelIndex_ReturnsPointerIntoSlice(t *testing.T) {
	t.Parallel()

	meta := mustScan(t, &idxUser{})
	for i := range meta.Fields {
		want := &meta.Fields[i]
		if got := meta.FieldByDBName(want.Tags.DBName); got != want {
			// Only compare when this field owns the name — a shadowed duplicate
			// legitimately resolves elsewhere. idxUser has no duplicates.
			t.Errorf("FieldByDBName(%q) = %p, want &Fields[%d] = %p", want.Tags.DBName, got, i, want)
		}
	}
	for i := range meta.Relations {
		want := &meta.Relations[i]
		if got := meta.RelationByKey(want.RelationKey); got != want {
			t.Errorf("RelationByKey(%q) = %p, want &Relations[%d] = %p", want.RelationKey, got, i, want)
		}
	}
}

// Two fields can carry the same DB column (an embed shadowing a BaseModel one —
// MS-L12, still open). The scan returned the first; a map built the obvious way
// would return the last and quietly move which field every caller resolves to.
func TestModelIndex_DuplicateNameResolvesToFirst(t *testing.T) {
	t.Parallel()

	strT := reflect.TypeOf("")
	meta := &ModelMeta{
		Name: "dup",
		Fields: []FieldMeta{
			{Name: "First", Type: strT, Tags: FieldTags{DBName: "name", JSONName: "name"}},
			{Name: "Second", Type: strT, Tags: FieldTags{DBName: "name", JSONName: "name"}},
		},
		Relations: []RelationMeta{
			{FieldName: "FirstRel", RelationKey: "rel", RelatedModel: "Target"},
			{FieldName: "SecondRel", RelationKey: "rel", RelatedModel: "Target"},
		},
	}

	if f := meta.FieldByDBName("name"); f == nil || f.Name != "First" {
		t.Errorf("FieldByDBName(\"name\") = %v, want the first field declaring the column", f)
	}
	if f := meta.FieldByJSONName("name"); f == nil || f.Name != "First" {
		t.Errorf("FieldByJSONName(\"name\") = %v, want the first field declaring the name", f)
	}
	if r := meta.RelationByKey("rel"); r == nil || r.FieldName != "FirstRel" {
		t.Errorf("RelationByKey(\"rel\") = %v, want the first relation declaring the key", r)
	}
	if r := meta.RelationByModel("Target"); r == nil || r.FieldName != "FirstRel" {
		t.Errorf("RelationByModel(\"Target\") = %v, want the first relation declaring the model", r)
	}
}

// ModelMeta is assembled by hand in places that never call ScanModel — the
// history model (newHistoryMeta), and test fixtures inside and outside this
// package. Indexing at registration would leave all of them on the slow path;
// worse, anything keyed off "was this scanned" would be a correctness trap.
func TestModelIndex_HandBuiltMeta(t *testing.T) {
	t.Parallel()

	meta := &ModelMeta{Name: "hand", Fields: []FieldMeta{
		{Name: "ID", Type: reflect.TypeOf(""), Tags: FieldTags{DBName: "id", JSONName: "id"}},
	}}
	if f := meta.FieldByDBName("id"); f == nil || f.Name != "ID" {
		t.Fatalf("FieldByDBName(\"id\") on a hand-built meta = %v, want the ID field", f)
	}

	// The zero ModelMeta has no fields at all and must stay total.
	empty := &ModelMeta{}
	if f := empty.FieldByDBName("id"); f != nil {
		t.Errorf("FieldByDBName on an empty meta = %v, want nil", f)
	}
	if r := empty.RelationByKey("x"); r != nil {
		t.Errorf("RelationByKey on an empty meta = %v, want nil", r)
	}
}

// resolveManyToMany appends relations when the router is built, and does it via
// addM2MIfMissing — which calls RelationByKey/RelationByModel first, building the
// index, and then appends. An index that did not notice the append would report
// the just-added relation as missing and let the next junction add it twice.
func TestModelIndex_SeesRelationsAppendedAfterFirstLookup(t *testing.T) {
	t.Parallel()

	meta := &ModelMeta{Name: "m2m"}

	if r := meta.RelationByKey("tags"); r != nil { // builds the index over zero relations
		t.Fatalf("RelationByKey(\"tags\") = %v before the relation exists, want nil", r)
	}
	meta.Relations = append(meta.Relations, RelationMeta{
		RelationKey: "tags", RelatedModel: "Tag", Kind: ManyToMany,
	})

	if r := meta.RelationByKey("tags"); r == nil {
		t.Error("RelationByKey(\"tags\") = nil after the relation was appended — the index went " +
			"stale, so resolveManyToMany would register the relation a second time")
	}
	if r := meta.RelationByModel("Tag"); r == nil {
		t.Error("RelationByModel(\"Tag\") = nil after the relation was appended — stale index")
	}
}

// A grown Fields slice must be picked up on the same terms.
func TestModelIndex_SeesFieldsAppendedAfterFirstLookup(t *testing.T) {
	t.Parallel()

	meta := &ModelMeta{Name: "grow"}
	if f := meta.FieldByDBName("added"); f != nil {
		t.Fatalf("FieldByDBName(\"added\") = %v before the field exists, want nil", f)
	}
	meta.Fields = append(meta.Fields, FieldMeta{
		Name: "Added", Type: reflect.TypeOf(""), Tags: FieldTags{DBName: "added", JSONName: "added"},
	})
	if f := meta.FieldByDBName("added"); f == nil {
		t.Error("FieldByDBName(\"added\") = nil after the field was appended — stale index")
	}
}

// The point of the change: the index is built once and reused, not rebuilt per
// lookup. Writing a bogus entry into the live index is only visible to a later
// lookup if that lookup reuses it.
func TestModelIndex_BuiltOnce(t *testing.T) {
	t.Parallel()

	meta := mustScan(t, &idxUser{})

	ix := meta.index()
	if again := meta.index(); again != ix {
		t.Fatal("index() rebuilt on the second call — the index is not memoised")
	}

	ix.fieldByDB["sentinel"] = 0
	if f := meta.FieldByDBName("sentinel"); f == nil {
		t.Error("FieldByDBName re-derived the index instead of reusing the built one")
	}
	if f := meta.FieldByDBName("email"); f == nil { // real lookups still work
		t.Error("FieldByDBName(\"email\") = nil")
	}
}

// ModelMeta is shared by every in-flight request for the model, so the build has
// to be safe under concurrent first lookups.
func TestModelIndex_ConcurrentLookups(t *testing.T) {
	t.Parallel()

	meta := mustScan(t, &idxUser{})
	const n = 64
	got := make([]*FieldMeta, n)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			got[i] = meta.FieldByJSONName("email")
		})
	}
	close(start)
	wg.Wait()

	want := &meta.Fields[0]
	for i, f := range got {
		if f == nil || f.Name != "Email" {
			t.Fatalf("goroutine %d resolved \"email\" to %v, want the Email field", i, f)
		}
		want = f
	}
	if meta.FieldByJSONName("email") != want {
		t.Error("a later lookup disagreed with the concurrent ones")
	}
}

// ── A/B ───────────────────────────────────────────────────────────────────────
// Separate benchmark runs on this machine drift by ~3x, so both arms live in one
// binary and one run.
//
//	go test -run '^$' -bench BenchmarkFieldLookupAB -benchmem

func scanFieldByDB(m *ModelMeta, name string) *FieldMeta {
	for i := range m.Fields {
		if m.Fields[i].Tags.DBName == name {
			return &m.Fields[i]
		}
	}
	return nil
}

func scanFieldByJSON(m *ModelMeta, name string) *FieldMeta {
	for i := range m.Fields {
		if m.Fields[i].Tags.JSONName == name {
			return &m.Fields[i]
		}
	}
	return nil
}

func benchMeta(nFields int) *ModelMeta {
	strT := reflect.TypeOf("")
	m := &ModelMeta{Name: "bench"}
	for i := range nFields {
		n := fmt.Sprintf("field_%02d", i)
		m.Fields = append(m.Fields, FieldMeta{
			Name: n, Type: strT, Tags: FieldTags{DBName: n, JSONName: n},
		})
	}
	return m
}

func BenchmarkFieldLookupAB(b *testing.B) {
	for _, n := range []int{8, 40} { // a small model and a wide one
		meta := benchMeta(n)
		// The last field and a miss are both full-length walks for the scan —
		// and the validate step's DeleteField takes the miss for every field the
		// body does not carry, which on a PATCH is nearly all of them.
		last := meta.Fields[n-1].Tags.DBName

		run := func(b *testing.B, f func(*ModelMeta, string) *FieldMeta) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				benchSink = f(meta, last)
				benchSink = f(meta, "absent")
			}
		}

		b.Run(fmt.Sprintf("scan/%dfields", n), func(b *testing.B) { run(b, scanFieldByDB) })
		b.Run(fmt.Sprintf("index/%dfields", n), func(b *testing.B) { run(b, (*ModelMeta).FieldByDBName) })
	}
}
