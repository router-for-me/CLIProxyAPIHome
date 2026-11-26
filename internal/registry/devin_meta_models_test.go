package registry

import "testing"

func TestGetDevinAndMetaModels(t *testing.T) {
	devinModels := GetDevinModels()
	if len(devinModels) == 0 {
		t.Fatal("GetDevinModels() returned empty list")
	}
	foundSwe2 := false
	for _, model := range devinModels {
		if model != nil && model.ID == "swe-2" {
			foundSwe2 = true
			break
		}
	}
	if !foundSwe2 {
		t.Fatal("expected swe-2 in GetDevinModels()")
	}

	metaModels := GetMetaModels()
	if len(metaModels) == 0 {
		t.Fatal("GetMetaModels() returned empty list")
	}
	foundMuse := false
	for _, model := range metaModels {
		if model != nil && model.ID == "muse-spark-1.3" {
			foundMuse = true
			break
		}
	}
	if !foundMuse {
		t.Fatal("expected muse-spark-1.3 in GetMetaModels()")
	}

	if defs := GetStaticModelDefinitionsByChannel("devin"); len(defs) == 0 {
		t.Fatal("GetStaticModelDefinitionsByChannel(devin) returned no models")
	}
	if defs := GetStaticModelDefinitionsByChannel("meta"); len(defs) == 0 {
		t.Fatal("GetStaticModelDefinitionsByChannel(meta) returned no models")
	}
	if defs := GetStaticModelDefinitionsByChannel("muse"); len(defs) == 0 {
		t.Fatal("GetStaticModelDefinitionsByChannel(muse) returned no models")
	}
}

func TestValidateModelsCatalogOptionalDevinMetaSections(t *testing.T) {
	base := *getModels()
	if err := validateModelsCatalog(&base); err != nil {
		t.Fatalf("validateModelsCatalog(base) error = %v", err)
	}

	emptyOptional := base
	emptyOptional.Devin = nil
	emptyOptional.Meta = nil
	if err := validateModelsCatalog(&emptyOptional); err != nil {
		t.Fatalf("validateModelsCatalog(empty optional) error = %v", err)
	}

	invalidDevin := base
	invalidDevin.Devin = []*ModelInfo{{ID: ""}}
	if err := validateModelsCatalog(&invalidDevin); err == nil {
		t.Fatal("validateModelsCatalog(invalid devin) = nil, want error")
	}

	duplicateMeta := base
	duplicateMeta.Meta = []*ModelInfo{
		{ID: "muse-spark-1.3"},
		{ID: "muse-spark-1.3"},
	}
	if err := validateModelsCatalog(&duplicateMeta); err == nil {
		t.Fatal("validateModelsCatalog(duplicate meta) = nil, want error")
	}
}
