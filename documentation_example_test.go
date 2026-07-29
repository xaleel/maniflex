package maniflex_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
)

func ExampleAdaptAuth() {
	// ANCHOR: adapt-auth
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
	// ANCHOR_END: adapt-auth

	req := httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	fmt.Println(rec.Code)

	// Output: 401
}
