package cluster

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestUsageObservabilityOverviewClientGroupsPostgres(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	deletedAt := gorm.DeletedAt{Time: from.Add(-time.Hour), Valid: true}
	users := []UserRecord{
		{Username: "original-owner"}, {Username: "current-owner"},
		{Username: "deleted-owner", DeletedAt: deletedAt}, {Username: ""},
	}
	if errCreate := db.Create(&users).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	missingUserID, historicalKeyID, missingKeyID := uint(80), uint(90), uint(99)
	keys := []APIKeyRecord{
		{APIKey: "fixture-k1", UserID: &users[1].ID},
		{APIKey: "fixture-k2", UserID: &users[0].ID},
		{APIKey: "fixture-k3"},
		{APIKey: "fixture-k4", UserID: &users[2].ID, DeletedAt: deletedAt},
		{APIKey: "fixture-k5", UserID: &missingUserID},
		{APIKey: "fixture-k6", UserID: &users[3].ID},
	}
	if errCreate := db.Create(&keys).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	latencies := make([]int64, 60)
	for index := range latencies {
		latencies[index] = int64(index/2) * 100
	}
	scenarios := []struct {
		name      string
		key       string
		billed    bool
		userID    *uint
		keyID     *uint
		latencies []int64
	}{
		{"frozen-owner", keys[0].APIKey, true, &users[0].ID, &historicalKeyID, latencies},
		{"unbilled", keys[0].APIKey, false, nil, nil, []int64{0, 100, 200}},
		{"null-user", keys[0].APIKey, true, nil, &historicalKeyID, []int64{1000, 5000}},
		{"null-key", keys[0].APIKey, true, &users[0].ID, nil, []int64{100, 200}},
		{"soft-deleted", keys[3].APIKey, false, nil, nil, []int64{-1, 0, 20}},
		{"empty-username", keys[5].APIKey, false, nil, nil, []int64{40}},
		{"missing-identities", "fixture-missing", true, &missingUserID, &missingKeyID, []int64{50, 60}},
		{"unknown", "fixture-missing", false, nil, nil, []int64{-1, -2}},
		{"negative-only", keys[2].APIKey, false, nil, nil, []int64{-1}},
		{"zero-only", keys[4].APIKey, false, nil, nil, []int64{0}},
	}
	var nullLabelIDs, nullMaskIDs []uint
	for _, scenario := range scenarios {
		for index, latency := range scenario.latencies {
			record := UsageRecord{
				Timestamp: from.Add(time.Duration(index) * time.Second), CreatedAt: from,
				APIKey: scenario.key, Model: scenario.name, Provider: "claude", ExecutorType: "ClaudeExecutor",
				LatencyMS: latency, Failed: index%5 == 0, PayloadJSON: JSONB(`{}`),
				InputTokens: int64(index + 10), OutputTokens: 5, ReasoningTokens: 2,
				CachedTokens: 3, CacheCreationTokens: 4, CacheReadTokensPresent: index%2 == 0,
			}
			if index%3 != 0 {
				record.TokenAccountingVersion = UsageTokenAccountingSchemaVersion
				record.TokenAccountingQuality = UsageTokenAccountingQualityComplete
				record.AccountingInputTokens, record.AccountingOutputTokens = 100, 20
				record.AccountingTotalTokens = 120
				record.UncachedInputTokens, record.AccountingCacheReadTokens, record.AccountingCacheWriteTokens = 85, 10, 5
				record.NonReasoningOutputTokens, record.AccountingReasoningTokens = 18, 2
			}
			if index%11 == 0 {
				record.TokenAccountingQuality = UsageTokenAccountingQualityInconsistent
			} else if index%13 == 0 {
				record.TokenAccountingQuality = UsageTokenAccountingQualityUnclassified
				record.UnclassifiedTokens = 7
			}
			if errCreate := db.Create(&record).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			if !scenario.billed {
				continue
			}
			charge := BillingChargeRecord{
				ID: fmt.Sprintf("charge-%d", record.ID), UsageID: record.ID, PayloadHash: fmt.Sprintf("hash-%d", record.ID),
				UserID: scenario.userID, APIKeyID: scenario.keyID,
				APIKeyLabel:  []string{"", "", "alpha", "zeta", "label", " "}[index%6],
				APIKeyMasked: []string{"", "", "zz-mask", "aa-mask"}[index%4],
				Amount:       float64(index%8) / 8, PriceSnapshot: JSONB(`{}`), CreatedAt: record.Timestamp,
			}
			if errCreate := db.Create(&charge).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			if index%6 == 0 {
				nullLabelIDs = append(nullLabelIDs, record.ID)
			}
			if index%4 == 0 {
				nullMaskIDs = append(nullMaskIDs, record.ID)
			}
		}
	}
	// NULL and empty labels have different fallback rules in historical charges.
	if errUpdate := db.Model(&BillingChargeRecord{}).Where("usage_id IN ?", nullLabelIDs).UpdateColumn("api_key_label", nil).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errUpdate := db.Model(&BillingChargeRecord{}).Where("usage_id IN ?", nullMaskIDs).UpdateColumn("api_key_masked", nil).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	for _, timestamp := range []time.Time{from.Add(-time.Second), to} {
		record := UsageRecord{Timestamp: timestamp, APIKey: keys[0].APIKey, Model: "frozen-owner", LatencyMS: 999999, PayloadJSON: JSONB(`{}`)}
		if errCreate := db.Create(&record).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
	}
	for _, model := range []string{"", "frozen-owner", "null-user", "null-key", "soft-deleted", "empty-username", "unknown", "negative-only", "zero-only", "absent"} {
		t.Run("model="+model, func(t *testing.T) {
			got := assertOverviewClientGroupsPostgres(t, db, UsageObservabilityRecordQuery{From: &from, To: &to, Model: model})
			switch model {
			case "frozen-owner":
				if len(got.Users) != 1 || got.Users[0].ID != strconv.FormatUint(uint64(users[0].ID), 10) || got.Users[0].RequestCount != 60 {
					t.Fatalf("lost billed ownership or time bounds: %+v", got.Users)
				}
				if len(got.ClientKeys) != 1 || got.ClientKeys[0].ID != "90" || got.ClientKeys[0].P95LatencyMS == nil || *got.ClientKeys[0].P95LatencyMS != 2800 {
					t.Fatalf("lost historical key or exact nearest-rank P95: %+v", got.ClientKeys)
				}
			case "unknown", "negative-only":
				if len(got.Users) != 1 || got.Users[0].ID != "unknown" || got.Users[0].P95LatencyMS != nil || got.Users[0].TotalAmount != nil {
					t.Fatalf("unknown/negative-only group changed: %+v", got.Users)
				}
			case "zero-only":
				if len(got.ClientKeys) != 1 || got.ClientKeys[0].P95LatencyMS == nil || *got.ClientKeys[0].P95LatencyMS != 0 {
					t.Fatalf("zero latency must remain a percentile sample: %+v", got.ClientKeys)
				}
			case "absent":
				if len(got.Users) != 0 || len(got.ClientKeys) != 0 {
					t.Fatalf("empty window produced groups: %+v", got)
				}
			}
		})
	}
}

func TestUsageObservabilityOverviewClientGroupsPostgresTopN(t *testing.T) {
	repo := newPostgresQuiescenceRepository(t)
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var records []UsageRecord
	for index := 1; index <= 14; index++ {
		// Reverse user labels relative to key labels so each Top N has different IDs.
		user := UserRecord{ID: uint(index), Username: fmt.Sprintf("user-%02d", 15-index)}
		if errCreate := db.Create(&user).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
		key := APIKeyRecord{ID: uint(index), APIKey: fmt.Sprintf("fixture-key-%d", index), UserID: &user.ID}
		if errCreate := db.Create(&key).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
		record := UsageRecord{Timestamp: at, APIKey: key.APIKey, LatencyMS: int64(index), PayloadJSON: JSONB(`{}`)}
		if errCreate := db.Create(&record).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
		records = append(records, record)
		charge := BillingChargeRecord{ID: fmt.Sprintf("charge-%d", index), UsageID: record.ID, PayloadHash: fmt.Sprintf("hash-%d", index),
			UserID: &user.ID, APIKeyID: &key.ID, APIKeyLabel: fmt.Sprintf("key-%02d", index), PriceSnapshot: JSONB(`{}`), CreatedAt: at}
		if errCreate := db.Create(&charge).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
	}
	got := assertOverviewClientGroupsPostgres(t, db, UsageObservabilityRecordQuery{})
	if len(got.Users) != 10 || len(got.ClientKeys) != 10 {
		t.Fatalf("each ranking must have its own Top 10: users=%d keys=%d", len(got.Users), len(got.ClientKeys))
	}
	for index := 0; index < 10; index++ {
		if got.Users[index].ID != strconv.Itoa(14-index) || got.ClientKeys[index].ID != strconv.Itoa(index+1) {
			t.Fatalf("label ordering must precede Top N: users=%+v keys=%+v", got.Users, got.ClientKeys)
		}
	}
	if errUpdate := db.Model(&UsageRecord{}).Where("id = ?", records[1].ID).UpdateColumn("timestamp", at.Add(time.Second)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	got = assertOverviewClientGroupsPostgres(t, db, UsageObservabilityRecordQuery{})
	if got.Users[0].ID != "2" || got.ClientKeys[0].ID != "2" {
		t.Fatalf("last_used_at must precede label ordering: users=%+v keys=%+v", got.Users, got.ClientKeys)
	}

	var rows []usageObservabilityAggregateRow
	statement := usageObservabilityOverviewClientGroupsPostgres(db.Session(&gorm.Session{DryRun: true}), UsageObservabilityRecordQuery{}).Find(&rows).Statement
	sql := statement.SQL.String()
	for _, fragment := range []string{`FROM "usage"`, `LEFT JOIN "billing_charge"`, `LEFT JOIN "api_key"`, `LEFT JOIN "user"`, "GROUPING SETS", "percentile_disc("} {
		if strings.Count(sql, fragment) != 1 {
			t.Fatalf("expected one %q in combined ranking SQL: %s", fragment, sql)
		}
	}
	if strings.Index(sql, `LEFT JOIN "user"`) < strings.Index(sql, "GROUPING SETS") {
		t.Fatalf("usernames must be joined after aggregation: %s", sql)
	}
}

// Compare public results against the independent single-group query path and
// ensure both rankings, including their percentiles, use one database statement.
func assertOverviewClientGroupsPostgres(t *testing.T, db *gorm.DB, query UsageObservabilityRecordQuery) UsageObservabilityTopGroups {
	t.Helper()
	counter := &overviewQueryCounter{Interface: logger.Default.LogMode(logger.Silent)}
	var rows []struct {
		Kind int                            `gorm:"column:kind"`
		Row  usageObservabilityAggregateRow `gorm:"embedded"`
	}
	if errScan := usageObservabilityOverviewClientGroupsPostgres(db.Session(&gorm.Session{Logger: counter}), query).Scan(&rows).Error; errScan != nil {
		t.Fatal(errScan)
	}
	if counter.count != 1 {
		t.Fatalf("combined rankings executed %d statements, want 1", counter.count)
	}
	got := UsageObservabilityTopGroups{Users: []UsageObservabilityAggregateItem{}, ClientKeys: []UsageObservabilityAggregateItem{}}
	for _, row := range rows {
		switch row.Kind {
		case 0:
			got.Users = append(got.Users, usageObservabilityAggregateItemFromRow(&row.Row, "user"))
		case 1:
			got.ClientKeys = append(got.ClientKeys, usageObservabilityAggregateItemFromRow(&row.Row, "client_key"))
		default:
			t.Fatalf("unexpected grouping discriminator %d", row.Kind)
		}
	}
	for _, group := range []struct {
		name  string
		items []UsageObservabilityAggregateItem
	}{{"user", got.Users}, {"client_key", got.ClientKeys}} {
		want, errWant := usageObservabilityAggregateItemsSQL(db, query, group.name, "request_count", "desc", 10)
		if errWant != nil {
			t.Fatal(errWant)
		}
		assertOverviewJSONEqual(t, group.name, group.items, want)
	}
	return got
}
