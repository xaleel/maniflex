package maniflex

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// warnDanglingRelations must warn for a convention "<Name>ID" relation whose
// target model is unregistered, but stay silent for a registered target and for
// an explicit (non-convention) relation.
func TestWarnDanglingRelations(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddForTest(&ModelMeta{
		Name: "Thread",
		Relations: []RelationMeta{
			{FieldName: "OwnerID", RelatedModel: "Owner", Convention: true, Kind: BelongsTo},     // unregistered → warn
			{FieldName: "AccountID", RelatedModel: "Account", Convention: true, Kind: BelongsTo},  // registered → quiet
			{FieldName: "ManagerID", RelatedModel: "Ghost", Convention: false, Kind: BelongsTo},   // explicit → quiet
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.AddForTest(&ModelMeta{Name: "Account"}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	warnDanglingRelations(reg, logger)

	out := buf.String()
	if !strings.Contains(out, "OwnerID") || !strings.Contains(out, "Owner") {
		t.Fatalf("expected a warning naming the dangling OwnerID→Owner relation, got: %q", out)
	}
	if !strings.Contains(out, "norelation") {
		t.Errorf("warning should suggest the norelation tag, got: %q", out)
	}
	if strings.Contains(out, "Account") {
		t.Errorf("registered target Account must not warn: %q", out)
	}
	if strings.Contains(out, "Ghost") {
		t.Errorf("explicit (non-convention) relation must not warn: %q", out)
	}
}
