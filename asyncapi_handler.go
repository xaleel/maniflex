package maniflex

import (
	"encoding/json"
	"net/http"
)

// asyncAPIHandler serves the AsyncAPI 2.6 document at {PathPrefix}/asyncapi.json.
// It is mounted only when Server.RealtimeDoc was called, so apps that don't use
// the realtime hub gain no new endpoint. The document is regenerated per request
// (cheap, and keeps it in sync if models are registered late) and emitted with
// a permissive CORS header so AsyncAPI Studio / codegen tools can load it
// cross-origin, matching the /openapi.json behaviour.
func asyncAPIHandler(reg RegistryAccessor, cfg *Config, asyncCfg AsyncAPIConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		spec := GenerateAsyncAPI(reg, cfg, asyncCfg)
		b, err := json.MarshalIndent(spec, "", "  ")
		if err != nil {
			http.Error(w,
				`{"error":{"code":"MARSHAL_ERROR","message":"failed to encode AsyncAPI document"}}`,
				http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		w.Write(b) //nolint:errcheck
	}
}
