package maniflex

// AsyncAPISpec is the root AsyncAPI 2.6.0 document describing the realtime event
// channels a client can subscribe to over the WebSocket / SSE hub. It is the
// event-stream analogue of OpenAPISpec: clients codegen typed event payloads
// from it the same way they codegen REST types from /openapi.json.
type AsyncAPISpec struct {
	AsyncAPI   string                     `json:"asyncapi"`
	Info       AsyncAPIInfo               `json:"info"`
	Servers    map[string]AsyncAPIServer  `json:"servers,omitempty"`
	Channels   map[string]AsyncAPIChannel `json:"channels"`
	Components AsyncAPIComponents         `json:"components,omitempty"`
}

// AsyncAPIInfo is the Info Object.
type AsyncAPIInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

// AsyncAPIServer is a Server Object (AsyncAPI 2.6 shape: url + protocol).
type AsyncAPIServer struct {
	URL         string `json:"url"`
	Protocol    string `json:"protocol"`
	Description string `json:"description,omitempty"`
}

// AsyncAPIChannel is a Channel Item Object. Each channel is one event type.
// The application sends, so events the client receives carry a `subscribe`
// operation (AsyncAPI 2.x perspective is the application's).
type AsyncAPIChannel struct {
	Description string             `json:"description,omitempty"`
	Subscribe   *AsyncAPIOperation `json:"subscribe,omitempty"`
	Publish     *AsyncAPIOperation `json:"publish,omitempty"`
}

// AsyncAPIOperation is an Operation Object.
type AsyncAPIOperation struct {
	OperationID string              `json:"operationId,omitempty"`
	Summary     string              `json:"summary,omitempty"`
	Description string              `json:"description,omitempty"`
	Message     *AsyncAPIMessageRef `json:"message,omitempty"`
}

// AsyncAPIMessageRef references a reusable message in components.messages.
type AsyncAPIMessageRef struct {
	Ref string `json:"$ref,omitempty"`
}

// AsyncAPIMessage is a Message Object. The payload reuses OASSchema because
// AsyncAPI 2.6 message payloads are JSON-Schema-shaped (the default schema
// format is a JSON Schema superset), so the same reflection that builds OpenAPI
// schemas serialises unchanged here.
type AsyncAPIMessage struct {
	Name        string     `json:"name,omitempty"`
	Title       string     `json:"title,omitempty"`
	Summary     string     `json:"summary,omitempty"`
	Description string     `json:"description,omitempty"`
	ContentType string     `json:"contentType,omitempty"`
	Payload     *OASSchema `json:"payload,omitempty"`
}

// AsyncAPIComponents holds reusable messages and schemas.
type AsyncAPIComponents struct {
	Messages map[string]*AsyncAPIMessage `json:"messages,omitempty"`
	Schemas  map[string]*OASSchema       `json:"schemas,omitempty"`
}
