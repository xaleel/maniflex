package maniflex

import (
	"fmt"
	"net/http/httptest"
	"strconv"
	"testing"
)

func arithmeticTestModel() *ModelMeta {
	return &ModelMeta{
		Name:      "ArithmeticTest",
		TableName: "arithmetic_tests",
		Fields: []FieldMeta{{
			Name: "Status",
			Tags: FieldTags{
				DBName:     "status",
				JSONName:   "status",
				Filterable: true,
			},
		}},
	}
}

func TestParseQueryParamsPaginationBounds(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest("GET",
		fmt.Sprintf("/arithmetic_tests?page=%d&limit=%d", maxPage, maxLimit), nil)
	q, err := ParseQueryParams(req, arithmeticTestModel(), nil)
	if err != nil {
		t.Fatalf("maximum page rejected: %v", err)
	}
	wantOffset := (maxPage - 1) * maxLimit
	if got := q.Offset(); got != wantOffset {
		t.Fatalf("Offset() = %d, want %d", got, wantOffset)
	}

	req = httptest.NewRequest("GET",
		fmt.Sprintf("/arithmetic_tests?page=%d", maxPage+1), nil)
	if _, err := ParseQueryParams(req, arithmeticTestModel(), nil); err == nil {
		t.Fatalf("page above maximum %d was accepted", maxPage)
	}
}

func TestQueryParamsOffsetNeverWraps(t *testing.T) {
	t.Parallel()

	maxInt := int(^uint(0) >> 1)
	if got := (&QueryParams{Page: maxInt, Limit: maxInt}).Offset(); got != maxInt {
		t.Fatalf("overflowing Offset() = %d, want saturation at %d", got, maxInt)
	}
	if got := (&QueryParams{Page: 3, Limit: 20}).Offset(); got != 40 {
		t.Fatalf("ordinary Offset() = %d, want 40", got)
	}
}

func TestParseQueryParamsFilterGroupIndexBounds(t *testing.T) {
	t.Parallel()

	maxInt := int(^uint(0) >> 1)
	req := httptest.NewRequest("GET", fmt.Sprintf(
		"/arithmetic_tests?filter%%5B%d%%5D=status:eq:active", maxInt-1), nil)
	q, err := ParseQueryParams(req, arithmeticTestModel(), nil)
	if err != nil {
		t.Fatalf("largest increment-safe filter group rejected: %v", err)
	}
	if got := q.Filters[0].Group; got != maxInt {
		t.Fatalf("internal group = %d, want %d", got, maxInt)
	}

	req = httptest.NewRequest("GET", fmt.Sprintf(
		"/arithmetic_tests?filter%%5B%d%%5D=status:eq:active", maxInt), nil)
	if _, err := ParseQueryParams(req, arithmeticTestModel(), nil); err == nil {
		t.Fatal("filter group index that overflows during increment was accepted")
	}

	tooLarge := "1" + strconv.FormatInt(int64(maxInt), 10)
	req = httptest.NewRequest("GET", fmt.Sprintf(
		"/arithmetic_tests?filter%%5B%s%%5D=status:eq:active", tooLarge), nil)
	if _, err := ParseQueryParams(req, arithmeticTestModel(), nil); err == nil {
		t.Fatal("filter group index outside the platform int range was accepted")
	}
}

func TestListOpenAPIAdvertisesPageMaximum(t *testing.T) {
	t.Parallel()

	for _, param := range listParameters(arithmeticTestModel()) {
		if param.Name != "page" {
			continue
		}
		if param.Schema == nil || param.Schema.Maximum == nil ||
			*param.Schema.Maximum != float64(maxPage) {
			t.Fatalf("page maximum = %#v, want %d", param.Schema, maxPage)
		}
		return
	}
	t.Fatal("list OpenAPI parameters omitted page")
}
