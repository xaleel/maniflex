package admin

import (
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/xaleel/maniflex"
)

// prettify turns a snake_case identifier into a human-readable Title Case
// label, e.g. "blog_posts" → "Blog Posts", "user_id" → "User ID".
func prettify(s string) string {
	parts := strings.Fields(strings.ReplaceAll(s, "_", " "))
	for i, p := range parts {
		switch p {
		case "id":
			parts[i] = "ID"
		default:
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// cellString renders a JSON-decoded value as a compact table cell.
func cellString(v any) string {
	switch x := v.(type) {
	case nil:
		return "—"
	case string:
		if x == "" {
			return "—"
		}
		return truncate(x, 80)
	case bool:
		if x {
			return "✓"
		}
		return "✗"
	case float64:
		// JSON numbers decode as float64; show integers without a fraction.
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return truncate(fmt.Sprintf("%v", x), 80)
	}
}

// truncate shortens s to at most n runes, appending an ellipsis when cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// cloneValues returns a deep copy of v so callers can mutate it freely.
func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vals := range v {
		out[k] = append([]string(nil), vals...)
	}
	return out
}

// formValue renders a JSON-decoded value as the string an HTML input expects.
func formValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// apiSort converts an admin sort token ("name" / "-name") into the API's
// "field:asc" / "field:desc" syntax.
func apiSort(s string) string {
	if field, ok := strings.CutPrefix(s, "-"); ok {
		return field + ":desc"
	}
	return s + ":asc"
}

// apiFilter builds one "field:op:value" clause. String columns use a
// case-insensitive substring match; everything else uses equality.
func apiFilter(meta *maniflex.ModelMeta, field, value string) string {
	op := "eq"
	if f := meta.FieldByJSONName(field); f != nil &&
		len(f.Tags.Enum) == 0 && kindOf(f.Type) == reflect.String {
		return field + ":ilike:%" + value + "%"
	}
	return field + ":" + op + ":" + value
}

// sortOptions produces the entries of the list-view sort dropdown: ascending
// and descending for every sortable field, plus a default "unsorted" entry.
func sortOptions(meta *maniflex.ModelMeta, current string) []sortOption {
	out := []sortOption{{Value: "", Label: "Default order", Selected: current == ""}}
	for _, f := range meta.Fields {
		if !f.Tags.Sortable {
			continue
		}
		label := prettify(f.Tags.JSONName)
		out = append(out,
			sortOption{Value: f.Tags.JSONName, Label: label + " ↑", Selected: current == f.Tags.JSONName},
			sortOption{Value: "-" + f.Tags.JSONName, Label: label + " ↓", Selected: current == "-"+f.Tags.JSONName},
		)
	}
	return out
}

// enumOptions renders an enum's members as <option> values, prepending an
// empty choice for optional fields.
func enumOptions(enum []string, current string, required bool) []optionView {
	var out []optionView
	if !required {
		out = append(out, optionView{Value: "", Label: "— none —", Selected: current == ""})
	}
	for _, e := range enum {
		out = append(out, optionView{Value: e, Label: e, Selected: e == current})
	}
	return out
}

// parseForm parses an incoming form body, handling both urlencoded and
// multipart submissions.
func parseForm(r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return r.ParseMultipartForm(32 << 20)
	}
	return r.ParseForm()
}

// statusOf maps an error to an HTTP status for the error page.
func statusOf(err error) int {
	if ae, ok := err.(*apiError); ok && ae.Status >= 400 {
		return ae.Status
	}
	return http.StatusBadGateway
}

// formErrorText returns the banner message for a failed submission, or "" when
// every problem is already shown inline against its field.
func formErrorText(ae *apiError) string {
	if len(ae.Fields) > 0 {
		return "Some fields need attention."
	}
	return ae.Message
}

// fileURL builds a download URL for a stored file key, or "" when unset.
func fileURL(apiPrefix, key string) string {
	if key == "" {
		return ""
	}
	return strings.TrimRight(apiPrefix, "/") + "/files/" + key
}

// datetimeLocal reformats an RFC3339 timestamp for a datetime-local input.
func datetimeLocal(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Format("2006-01-02T15:04")
	}
	return s
}
