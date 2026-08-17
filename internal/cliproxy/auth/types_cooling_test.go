package auth

import "testing"

func TestDisableCoolingOverrideSupportsExplicitFalse(t *testing.T) {
	runtimeFalse := false
	runtimeTrue := true
	tests := []struct {
		name string
		auth *Auth
		want *bool
	}{
		{name: "unset", auth: &Auth{}},
		{name: "metadata true", auth: &Auth{Metadata: map[string]any{"disable_cooling": true}}, want: &runtimeTrue},
		{name: "metadata false", auth: &Auth{Metadata: map[string]any{"disable_cooling": false}}, want: &runtimeFalse},
		{name: "legacy metadata false", auth: &Auth{Metadata: map[string]any{"disable-cooling": false}}, want: &runtimeFalse},
		{name: "runtime false", auth: &Auth{RuntimeDisableCooling: &runtimeFalse}, want: &runtimeFalse},
		{name: "runtime true", auth: &Auth{RuntimeDisableCooling: &runtimeTrue}, want: &runtimeTrue},
		{name: "runtime false overrides metadata true", auth: &Auth{RuntimeDisableCooling: &runtimeFalse, Metadata: map[string]any{"disable_cooling": true}}, want: &runtimeFalse},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.auth.DisableCoolingOverride()
			if tc.want == nil {
				if got != nil {
					t.Fatalf("DisableCoolingOverride() = %#v, want nil", got)
				}
			} else if got == nil || *got != *tc.want {
				t.Fatalf("DisableCoolingOverride() = %#v, want %t", got, *tc.want)
			}
		})
	}
}

func TestAuthCloneCopiesRuntimeDisableCoolingOverride(t *testing.T) {
	disableCooling := false
	auth := &Auth{RuntimeDisableCooling: &disableCooling}
	cloned := auth.Clone()

	if cloned.RuntimeDisableCooling == nil || *cloned.RuntimeDisableCooling {
		t.Fatalf("cloned runtime override = %#v, want false", cloned.RuntimeDisableCooling)
	}
	if cloned.RuntimeDisableCooling == auth.RuntimeDisableCooling {
		t.Fatal("cloned runtime override shares its pointer with the source auth")
	}
}
