package config

import (
	"encoding/json"
	"reflect"
	"testing"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/registry"
	"gopkg.in/yaml.v3"
)

// Model definitions must remain complete even when only CPA interprets an option.
func TestModelConfigMatchesSDK(t *testing.T) {
	thinking := &registry.ThinkingSupport{
		Min: 128, Max: 8192, ZeroAllowed: true, DynamicAllowed: true,
		Levels: []string{"low", "high"},
	}
	openAPI, native := true, false
	for _, test := range []struct {
		name  string
		model any
		sdk   reflect.Type
	}{
		{
			name: "claude",
			model: ClaudeModel{
				Name: "upstream", Alias: "alias", DisplayName: "Model", MaxContextLength: 32768,
				ForceMapping: true, IsCompat: true, Thinking: thinking,
			},
			sdk: reflect.TypeOf(cpaconfig.ClaudeKey{}.Models).Elem(),
		},
		{
			name: "codex",
			model: CodexModel{
				Name: "upstream", Alias: "alias", DisplayName: "Model", MaxContextLength: 32768,
				ForceMapping: true, IsCompat: true, SupportConfigurationUpdate: true, Thinking: thinking,
			},
			sdk: reflect.TypeOf(cpaconfig.CodexKey{}.Models).Elem(),
		},
		{
			name: "gemini",
			model: GeminiModel{
				Name: "upstream", Alias: "alias", DisplayName: "Model", MaxContextLength: 32768,
				ForceMapping: true, IsCompat: true, Thinking: thinking,
			},
			sdk: reflect.TypeOf(cpaconfig.GeminiKey{}.Models).Elem(),
		},
		{
			name: "openai-compatibility",
			model: OpenAICompatibilityModel{
				Name: "upstream", Alias: "alias", DisplayName: "Model", MaxContextLength: 32768,
				ForceMapping: true, Image: true, InputModalities: []string{"text", "image"},
				OutputModalities: []string{"image"}, IsCompat: true, UseMaxCompletionTokens: true, Thinking: thinking,
			},
			sdk: reflect.TypeOf(cpaconfig.OpenAICompatibilityModel{}),
		},
		{
			name: "vertex-openapi",
			model: VertexCompatModel{
				Name: "upstream", Alias: "alias", DisplayName: "Model",
				ForceMapping: true, OpenAPI: &openAPI, Thinking: thinking,
			},
			sdk: reflect.TypeOf(cpaconfig.VertexCompatModel{}),
		},
		{
			name:  "vertex-native",
			model: VertexCompatModel{Name: "upstream", Alias: "alias", OpenAPI: &native},
			sdk:   reflect.TypeOf(cpaconfig.VertexCompatModel{}),
		},
		{
			name:  "vertex-default",
			model: VertexCompatModel{Name: "upstream", Alias: "alias"},
			sdk:   reflect.TypeOf(cpaconfig.VertexCompatModel{}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			homeType := reflect.TypeOf(test.model)
			if homeType.NumField() != test.sdk.NumField() {
				t.Errorf("Home has %d model fields, SDK has %d", homeType.NumField(), test.sdk.NumField())
			}
			for i := 0; i < test.sdk.NumField(); i++ {
				sdkField := test.sdk.Field(i)
				homeField, exists := homeType.FieldByName(sdkField.Name)
				if !exists {
					t.Errorf("Home is missing model field %s", sdkField.Name)
					continue
				}
				if homeField.Type.Kind() != sdkField.Type.Kind() {
					t.Errorf("field %s has kind %s, want %s", sdkField.Name, homeField.Type.Kind(), sdkField.Type.Kind())
				}
				for _, format := range []string{"json", "yaml"} {
					if got, want := homeField.Tag.Get(format), sdkField.Tag.Get(format); got != want {
						t.Errorf("field %s has %s tag %q, want %q", sdkField.Name, format, got, want)
					}
				}
			}
			for _, codec := range []struct {
				name      string
				marshal   func(any) ([]byte, error)
				unmarshal func([]byte, any) error
			}{
				{"json", json.Marshal, json.Unmarshal},
				{"yaml", yaml.Marshal, yaml.Unmarshal},
			} {
				t.Run(codec.name, func(t *testing.T) {
					data, errMarshal := codec.marshal(test.model)
					if errMarshal != nil {
						t.Fatal(errMarshal)
					}
					sdkModel := reflect.New(test.sdk).Interface()
					if errDecode := codec.unmarshal(data, sdkModel); errDecode != nil {
						t.Fatal(errDecode)
					}
					encoded, errEncode := codec.marshal(sdkModel)
					if errEncode != nil {
						t.Fatal(errEncode)
					}
					restored := reflect.New(homeType)
					if errDecode := codec.unmarshal(encoded, restored.Interface()); errDecode != nil {
						t.Fatal(errDecode)
					}
					if !reflect.DeepEqual(test.model, restored.Elem().Interface()) {
						t.Fatalf("model configuration changed across SDK round trip: %s", encoded)
					}
				})
			}
		})
	}
}
