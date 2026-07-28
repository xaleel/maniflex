package maniflex_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
)

func ExampleAdaptAuth() {
	const jwtSecret = "replace-with-a-strong-32-byte-secret"
	server := maniflex.New(maniflex.Config{
		Documentation: maniflex.DocumentationConfig{
			Middleware: []maniflex.HTTPMiddleware{
				maniflex.AdaptAuth(
					auth.JWTAuth(jwtSecret),
					auth.RequireRole("internal"),
				),
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	fmt.Println(rec.Code)

	// Output: 401
}
