package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOAuthVertexOpenAPIAliasNormalization(t *testing.T) {
	for _, source := range []string{
		`oauth-model-alias:
  vertex:
    - {name: gemini-2.5-flash, alias: force-openapi, openapi: true}
    - {name: gemini-2.5-flash, alias: force-native, openapi: false}
    - {name: gemini-2.5-flash, alias: automatic}
    - {name: gemini-2.5-flash, alias: null-value, openapi: null}
`,
		`config-version: 8
oauth:
  model-alias:
    vertex:
      - {name: gemini-2.5-flash, alias: force-openapi, openapi: true}
      - {name: gemini-2.5-flash, alias: force-native, openapi: false}
      - {name: gemini-2.5-flash, alias: automatic}
      - {name: gemini-2.5-flash, alias: null-value, openapi: null}
`,
	} {
		var cfg Config
		if errDecode := yaml.Unmarshal([]byte(source), &cfg); errDecode != nil {
			t.Fatal(errDecode)
		}
		cfg.SanitizeOAuthModelAlias()
		data, errMarshal := json.Marshal(cfg.OAuthModelAlias["vertex"])
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		var aliases []struct {
			Alias   string `json:"alias"`
			OpenAPI *bool  `json:"openapi"`
		}
		if errDecode := json.Unmarshal(data, &aliases); errDecode != nil {
			t.Fatal(errDecode)
		}
		if len(aliases) != 4 || aliases[0].OpenAPI == nil || !*aliases[0].OpenAPI || aliases[1].OpenAPI == nil || *aliases[1].OpenAPI || aliases[2].OpenAPI != nil || aliases[3].OpenAPI != nil {
			t.Fatalf("normalization lost explicit Vertex routing flags: %s", data)
		}
	}
}
