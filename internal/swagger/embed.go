package swagger

import _ "embed"

// OpenAPISpec is the raw contents of geofence-openapi.yaml, embedded into the
// binary so the Swagger UI route can serve it without relying on the source
// tree being present at runtime (the deploy image only contains the compiled
// binary — see deploy/Dockerfile).
//
//go:embed geofence-openapi.yaml
var OpenAPISpec []byte
