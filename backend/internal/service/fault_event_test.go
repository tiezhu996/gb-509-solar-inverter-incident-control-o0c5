package service

import (
	"strings"
	"testing"
	"time"

	"github.com/blueship581/solar-inverter-incident-control/backend/internal/model"
)

func TestApplyDuplicateReportAccumulatesAndKeepsOpen(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	existing := model.FaultEvent{
		BaseModel:       model.BaseModel{Status: "open"},
		RiskLevel:       "medium",
		Evidence:        "首次告警证据",
		OccurrenceCount: 1,
		LastReportedAt:  base,
	}
	incoming := model.FaultEvent{
		RiskLevel:      "critical",
		Evidence:       "重复上报证据",
		LastReportedAt: base.Add(5 * time.Minute),
	}
	applyDuplicateReport(&existing, &incoming)
	if existing.OccurrenceCount != 2 {
		t.Fatalf("expected occurrence count 2, got %d", existing.OccurrenceCount)
	}
	if !existing.LastReportedAt.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("expected last reported time to advance, got %v", existing.LastReportedAt)
	}
	if existing.RiskLevel != "critical" {
		t.Fatalf("expected higher risk level critical, got %s", existing.RiskLevel)
	}
	if existing.Status != "open" {
		t.Fatalf("expected open event to stay open, got %s", existing.Status)
	}
	if !strings.Contains(existing.Evidence, "首次告警证据") || !strings.Contains(existing.Evidence, "重复上报证据") {
		t.Fatalf("expected merged evidence, got %q", existing.Evidence)
	}
}

func TestApplyDuplicateReportReopensAcknowledged(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	existing := model.FaultEvent{
		BaseModel:       model.BaseModel{Status: "acknowledged"},
		RiskLevel:       "high",
		OccurrenceCount: 3,
		LastReportedAt:  base,
	}
	incoming := model.FaultEvent{RiskLevel: "low", LastReportedAt: base.Add(-2 * time.Minute)}
	applyDuplicateReport(&existing, &incoming)
	if existing.Status != "open" {
		t.Fatalf("expected acknowledged event to reopen as open, got %s", existing.Status)
	}
	if existing.OccurrenceCount != 4 {
		t.Fatalf("expected occurrence count 4, got %d", existing.OccurrenceCount)
	}
	if !existing.LastReportedAt.Equal(base) {
		t.Fatalf("expected last reported time to stay at %v, got %v", base, existing.LastReportedAt)
	}
	if existing.RiskLevel != "high" {
		t.Fatalf("expected risk level to stay high, got %s", existing.RiskLevel)
	}
}

func TestWithinMergeWindow(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	if !withinMergeWindow(base, base.Add(faultMergeWindow)) {
		t.Fatal("expected report exactly at the window edge to merge")
	}
	if withinMergeWindow(base, base.Add(faultMergeWindow+time.Second)) {
		t.Fatal("expected report beyond the window to stay separate")
	}
	if withinMergeWindow(base, base.Add(-faultMergeWindow-time.Second)) {
		t.Fatal("expected stale report beyond the window to stay separate")
	}
}

func TestMergeEvidence(t *testing.T) {
	if got := mergeEvidence("已有证据", "已有证据"); got != "已有证据" {
		t.Fatalf("expected duplicate evidence to be skipped, got %q", got)
	}
	if got := mergeEvidence("", "新证据"); got != "新证据" {
		t.Fatalf("expected empty evidence to be replaced, got %q", got)
	}
	if got := mergeEvidence("已有证据", ""); got != "已有证据" {
		t.Fatalf("expected empty incoming evidence to be ignored, got %q", got)
	}
	long := strings.Repeat("证", 1999)
	if got := mergeEvidence(long, "新证据"); len([]rune(got)) != 2000 {
		t.Fatalf("expected merged evidence to be truncated to 2000 runes, got %d", len([]rune(got)))
	}
}
