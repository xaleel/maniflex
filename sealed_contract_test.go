package maniflex

// Audit DX1: the audit reads this as an inconsistent contract — three setters
// panicking where Register returns ErrRegistrationClosed. The split is actually
// principled: every method that returns ErrRegistrationClosed also validates
// something (Register alone has four error paths that have nothing to do with
// sealing), so the sealing check rides an error return that had to exist. The
// methods that only wire have nothing to validate, so their one failure mode is
// a programming error, which is what panic is for.
//
// What was really wrong is narrower: of the nine methods that refuse a late
// call, four panicked without saying so in their doc comment. Nothing checked,
// so they drifted. These tests are the check.
//
//	go test . -run TestSealedContract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// sealedGuarded is every Server method that refuses a call once the server is
// sealed, with a call that provokes it. Kept in sync with the source by
// TestSealedContract_ListIsComplete below, so a method added later cannot slip
// past the documentation check.
var sealedGuarded = []struct {
	name string
	call func(*Server)
}{
	{"AddService", func(s *Server) { s.AddService(ServiceFunc(nil)) }},
	{"SetStorage", func(s *Server) { s.SetStorage(nil) }},
	{"SetKeyProvider", func(s *Server) { s.SetKeyProvider(nil) }},
	{"SetDB", func(s *Server) { s.SetDB(nil) }},
	{"ObserveRequests", func(s *Server) { s.ObserveRequests() }},
	{"Action", func(s *Server) { s.Action(ActionConfig{}) }},
	{"AllowPublic", func(s *Server) { s.AllowPublic() }},
	{"RealtimeDoc", func(s *Server) { s.RealtimeDoc(AsyncAPIConfig{}) }},
	{"EnableGlobalSearch", func(s *Server) { s.EnableGlobalSearch() }},
}

// sealedServer returns a Server that is sealed by both measures: the state has
// moved off serverNew (which is what AddService checks) and a router exists
// (which sealedLocked also accepts).
func sealedServer() *Server {
	s := New(Config{})
	s.state = serverRunning
	s.router = http.NotFoundHandler()
	return s
}

// Every one of them must actually refuse. A method that silently accepted a
// late call would leave the fixed routes disagreeing with the live config.
func TestSealedContract_EveryGuardedMethodPanicsWhenSealed(t *testing.T) {
	for _, m := range sealedGuarded {
		t.Run(m.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("%s accepted a call on a sealed server", m.name)
				}
				msg, _ := r.(string)
				if !strings.Contains(msg, "maniflex: ") || !strings.Contains(msg, m.name) {
					t.Errorf("%s panicked with %q, which does not name the package and the "+
						"method: the whole value of panicking here is that the message says "+
						"which call was misplaced", m.name, msg)
				}
			}()
			m.call(sealedServer())
		})
	}
}

// The finding: four of these panicked with nothing in the doc comment saying so.
func TestSealedContract_EveryGuardedMethodDocumentsThePanic(t *testing.T) {
	docs := serverMethodDocs(t)
	for _, m := range sealedGuarded {
		doc, ok := docs[m.name]
		if !ok {
			t.Errorf("%s has no doc comment at all", m.name)
			continue
		}
		if !strings.Contains(strings.ToLower(doc), "panic") {
			t.Errorf("%s panics on a late call and its doc comment does not say so: the "+
				"contract is only discoverable by triggering it", m.name)
		}
	}
}

// And the list above must be the whole list, or the documentation check silently
// stops covering whatever was added last — which is how the four drifted.
func TestSealedContract_ListIsComplete(t *testing.T) {
	found := sealedGuardedInSource(t)

	listed := map[string]bool{}
	for _, m := range sealedGuarded {
		listed[m.name] = true
	}

	for _, name := range found {
		if !listed[name] {
			t.Errorf("%s refuses a late call but is not in sealedGuarded, so nothing checks "+
				"that it documents the panic — add it", name)
		}
	}
	for name := range listed {
		if !contains(found, name) {
			t.Errorf("sealedGuarded lists %s, but no panic in server.go names a sealing "+
				"violation for it — the entry is stale", name)
		}
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// sealedGuardedInSource finds every *Server method whose body panics with a
// message about being sealed or about ordering against Start/Handler. Matching
// on the message rather than on sealedLocked deliberately: AddService guards on
// a narrower condition of its own, and Action panics for unrelated reasons too.
func sealedGuardedInSource(t *testing.T) []string {
	t.Helper()
	file := parseServerGo(t)

	var names []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "panic" {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				if strings.Contains(s, "sealed") || strings.Contains(s, "before Start") {
					if !contains(names, fn.Name.Name) {
						names = append(names, fn.Name.Name)
					}
				}
			}
			return true
		})
	}
	sort.Strings(names)
	return names
}

func serverMethodDocs(t *testing.T) map[string]string {
	t.Helper()
	file := parseServerGo(t)
	docs := map[string]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			continue
		}
		if fn.Doc != nil {
			docs[fn.Name.Name] = fn.Doc.Text()
		}
	}
	return docs
}

func parseServerGo(t *testing.T) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "server.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing server.go: %v", err)
	}
	return file
}
