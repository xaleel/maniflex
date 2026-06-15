package maniflex

import (
	"reflect"
	"strings"
)

// EventDoc documents one realtime event type's payload for AsyncAPI generation.
// Payload accepts a struct value, pointer, or reflect.Type annotated with the
// same json + mfx tags as models; it is reflected into a JSON schema exactly
// like an action's RequestSchema/ResponseSchema (10.8).
type EventDoc struct {
	Type        string // CloudEvents type / topic, e.g. "invoice.created"
	Title       string
	Description string
	Payload     any
}

// AsyncAPIServerConfig describes one endpoint the realtime hub is mounted at.
// Name keys the server in the document (defaults to Protocol when empty).
type AsyncAPIServerConfig struct {
	Name        string
	URL         string // e.g. "ws://localhost:8080/ws" or "http://localhost:8080/sse"
	Protocol    string // "ws", "wss", "sse", "http"
	Description string
}

// AsyncAPIConfig is registered once via Server.RealtimeDoc to enable the
// {PathPrefix}/asyncapi.json endpoint. Title/Version default to the API name.
type AsyncAPIConfig struct {
	Title   string
	Version string
	Servers []AsyncAPIServerConfig

	// Events declares custom event types and their payloads.
	Events []EventDoc

	// AutoModelEvents derives <model>.created|updated|deleted channels from the
	// model registry, using each model's struct as the message payload. This
	// mirrors the default events.Emit type naming; apps that customise
	// EmitConfig.Type should declare their events explicitly via Events instead.
	AutoModelEvents bool
}

// modelEventSuffixes mirrors events.DefaultType's mutation suffixes. Duplicated
// here so the core package documents model events without importing events
// (which would be an import cycle — events imports maniflex).
var modelEventSuffixes = []string{"created", "updated", "deleted"}

// GenerateAsyncAPI builds an AsyncAPI 2.6 document from the model registry and
// the supplied AsyncAPIConfig. It is called by the asyncapi.json handler and
// can be called directly to obtain the base document for customisation.
func GenerateAsyncAPI(reg RegistryAccessor, cfg *Config, asyncCfg AsyncAPIConfig) *AsyncAPISpec {
	title := asyncCfg.Title
	if title == "" {
		title = "maniflex events"
	}
	version := asyncCfg.Version
	if version == "" {
		version = "1.0.0"
	}

	spec := &AsyncAPISpec{
		AsyncAPI: "2.6.0",
		Info: AsyncAPIInfo{
			Title:       title,
			Version:     version,
			Description: "Auto-generated AsyncAPI document for maniflex realtime events.",
		},
		Channels: make(map[string]AsyncAPIChannel),
		Components: AsyncAPIComponents{
			Messages: make(map[string]*AsyncAPIMessage),
			Schemas:  make(map[string]*OASSchema),
		},
	}

	if len(asyncCfg.Servers) > 0 {
		spec.Servers = make(map[string]AsyncAPIServer, len(asyncCfg.Servers))
		for _, s := range asyncCfg.Servers {
			name := s.Name
			if name == "" {
				name = s.Protocol
			}
			spec.Servers[name] = AsyncAPIServer{URL: s.URL, Protocol: s.Protocol, Description: s.Description}
		}
	}

	if asyncCfg.AutoModelEvents {
		addModelEvents(spec, reg)
	}

	for _, ed := range asyncCfg.Events {
		if ed.Type == "" {
			continue
		}
		addEventChannel(spec, ed.Type, ed.Title, ed.Description, buildEventPayload(spec, ed))
	}

	return spec
}

// addModelEvents adds <model>.created|updated|deleted channels for every
// registered model, reusing the model's reflected struct schema as the payload.
func addModelEvents(spec *AsyncAPISpec, reg RegistryAccessor) {
	for _, m := range reg.All() {
		schemaName := m.Name
		if _, ok := spec.Components.Schemas[schemaName]; !ok {
			if s := reflectTypeSchema(m.GoType, 0); s != nil {
				spec.Components.Schemas[schemaName] = s
			}
		}
		for _, suffix := range modelEventSuffixes {
			typ := lowerFirstByte(m.Name) + "." + suffix
			addEventChannel(spec, typ, m.Name+" "+suffix, m.Name+" "+suffix+" event", ref(schemaName))
		}
	}
}

// buildEventPayload reflects an EventDoc's Payload into a schema. Named struct
// types are hoisted into components.schemas and referenced; anonymous or scalar
// payloads are inlined.
func buildEventPayload(spec *AsyncAPISpec, ed EventDoc) *OASSchema {
	if ed.Payload == nil {
		return nil
	}
	s := reflectSchema(ed.Payload)
	if s == nil {
		return nil
	}
	if name := schemaTypeName(ed.Payload); name != "" {
		if _, ok := spec.Components.Schemas[name]; !ok {
			spec.Components.Schemas[name] = s
		}
		return ref(name)
	}
	return s
}

// addEventChannel registers a subscribe channel + reusable message for one
// event type. Component keys reuse the event type verbatim — AsyncAPI allows
// dots, dashes and underscores in component names, so "invoice.created" is a
// valid key and $ref target.
func addEventChannel(spec *AsyncAPISpec, eventType, title, desc string, payload *OASSchema) {
	if _, ok := spec.Components.Messages[eventType]; !ok {
		spec.Components.Messages[eventType] = &AsyncAPIMessage{
			Name:        eventType,
			Title:       title,
			Description: desc,
			ContentType: "application/json",
			Payload:     payload,
		}
	}
	spec.Channels[eventType] = AsyncAPIChannel{
		Description: desc,
		Subscribe: &AsyncAPIOperation{
			OperationID: "on" + pascalCase(eventType),
			Summary:     title,
			Message:     &AsyncAPIMessageRef{Ref: "#/components/messages/" + eventType},
		},
	}
}

// schemaTypeName returns the Go type name of a payload value/pointer/Type so
// the schema can be hoisted into components.schemas. Anonymous types yield "".
func schemaTypeName(v any) string {
	rt, ok := v.(reflect.Type)
	if !ok {
		rt = reflect.TypeOf(v)
	}
	for rt != nil && rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt == nil || rt.Kind() != reflect.Struct {
		return ""
	}
	return rt.Name()
}

// lowerFirstByte lowercases the first ASCII byte, matching events.DefaultType's
// model-name → type-prefix convention (e.g. "Invoice" → "invoice").
func lowerFirstByte(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'A' && b[0] <= 'Z' {
		b[0] += 32
	}
	return string(b)
}

// pascalCase joins the dot/dash/underscore-separated segments of an event type
// into a PascalCase identifier for use in operationIds (e.g. "invoice.created"
// → "InvoiceCreated").
func pascalCase(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '.' || r == '-' || r == '_' || r == '/'
	})
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(capitalize(p))
	}
	return b.String()
}
