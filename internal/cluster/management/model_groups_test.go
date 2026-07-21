package management

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestModelGroupDetailRecordToMapIncludesChannels(t *testing.T) {
	t.Parallel()

	item, errItem := modelGroupDetailRecordToMap(&cluster.ModelGroupDetailRecord{
		ID:           10,
		ModelGroupID: 2,
		ModelID:      "gpt-5.4",
		Channels:     cluster.JSONB(`[4,2,4]`),
	})
	if errItem != nil {
		t.Fatalf("modelGroupDetailRecordToMap() error = %v", errItem)
	}
	channels, ok := item["channels"].([]uint)
	if !ok {
		t.Fatalf("channels = %T, want []uint", item["channels"])
	}
	if want := []uint{2, 4}; !reflect.DeepEqual(channels, want) {
		t.Fatalf("channels = %v, want %v", channels, want)
	}
}
