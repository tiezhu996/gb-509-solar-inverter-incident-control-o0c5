package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/blueship581/solar-inverter-incident-control/backend/internal/config"
	"github.com/blueship581/solar-inverter-incident-control/backend/internal/dto"
	"github.com/blueship581/solar-inverter-incident-control/backend/internal/model"
	"github.com/blueship581/solar-inverter-incident-control/backend/internal/repository"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type stubSecurityService struct {
	actions []string
}

func (s *stubSecurityService) Login(context.Context, dto.LoginRequest) (dto.LoginResponse, error) {
	return dto.LoginResponse{}, nil
}

func (s *stubSecurityService) Audit(_ context.Context, _, _, action, _ string, _ uint, _, _, _ string) error {
	s.actions = append(s.actions, action)
	return nil
}

func (s *stubSecurityService) ListAudits(context.Context, int, int, string) ([]model.AuditLog, int64, error) {
	return nil, 0, nil
}

func (s *stubSecurityService) AuditSummary(context.Context, time.Duration) (model.AuditSummary, error) {
	return model.AuditSummary{}, nil
}

func (s *stubSecurityService) EntityHistory(context.Context, string, uint, int) ([]model.AuditLog, error) {
	return nil, nil
}

func (s *stubSecurityService) RuntimeConfig() config.PublicConfig { return config.PublicConfig{} }

func newFaultEventTestService(t *testing.T) (FaultEventService, *stubSecurityService) {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory database: %v", err)
	}
	if err := db.AutoMigrate(&model.FaultEvent{}); err != nil {
		t.Fatalf("migrate fault events: %v", err)
	}
	security := &stubSecurityService{}
	return NewFaultEventService(repository.NewFaultEventRepository(db), security), security
}

func faultReportInput(code string, reportedAt time.Time) dto.CreateFaultEvent {
	return dto.CreateFaultEvent{
		Code: code, Name: "逆变器过温告警", Description: "同一台逆变器连续上报",
		Facility: "华东一场站", Owner: "运行一组", Category: "过温",
		RiskLevel: "medium", MetricValue: 87.5, MetricUnit: "℃",
		EffectiveAt: reportedAt, Evidence: "测温探头读数 87.5℃", RelatedCode: "INV-01",
	}
}

func countFaultEvents(t *testing.T, svc FaultEventService) int64 {
	t.Helper()
	page, err := svc.List(context.Background(), dto.PageQuery{Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("list fault events: %v", err)
	}
	return page.Total
}

func TestFaultEventCreateMergesDuplicateWithinWindow(t *testing.T) {
	svc, security := newFaultEventTestService(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first, err := svc.Create(ctx, faultReportInput("FE-100", now), "operator", "req-1")
	if err != nil {
		t.Fatalf("create first report: %v", err)
	}
	if first.OccurrenceCount != 1 || !first.LastReportedAt.Equal(now) {
		t.Fatalf("expected initial occurrence=1 and lastReportedAt=now, got %+v", first)
	}

	second := faultReportInput("FE-101", now.Add(5*time.Minute))
	second.RiskLevel = "critical"
	second.Evidence = "测温探头读数 91.2℃"
	merged, err := svc.Create(ctx, second, "operator", "req-2")
	if err != nil {
		t.Fatalf("create duplicate report: %v", err)
	}
	if merged.ID != first.ID {
		t.Fatalf("expected duplicate to merge into event %d, got new event %d", first.ID, merged.ID)
	}
	if merged.OccurrenceCount != 2 {
		t.Fatalf("expected occurrence=2, got %d", merged.OccurrenceCount)
	}
	if !merged.LastReportedAt.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("expected lastReportedAt to advance, got %v", merged.LastReportedAt)
	}
	if merged.RiskLevel != "critical" {
		t.Fatalf("expected risk to escalate to critical, got %s", merged.RiskLevel)
	}
	if !strings.Contains(merged.Evidence, "87.5") || !strings.Contains(merged.Evidence, "91.2") {
		t.Fatalf("expected merged evidence to keep both fragments, got %q", merged.Evidence)
	}
	if merged.Status != "open" {
		t.Fatalf("expected merged event to stay open, got %s", merged.Status)
	}
	if total := countFaultEvents(t, svc); total != 1 {
		t.Fatalf("expected a single merged event, got %d", total)
	}
	if len(security.actions) != 2 || security.actions[1] != "merge" {
		t.Fatalf("expected create+merge audit actions, got %v", security.actions)
	}
}

func TestFaultEventAcknowledgedReportReopens(t *testing.T) {
	svc, _ := newFaultEventTestService(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first, err := svc.Create(ctx, faultReportInput("FE-200", now), "operator", "req-1")
	if err != nil {
		t.Fatalf("create first report: %v", err)
	}
	acknowledged, err := svc.Transition(ctx, first.ID, dto.TransitionRequest{
		Status: "acknowledged", ExpectedVersion: first.Version, Reason: "值班员已认领",
	}, "operator", "req-2")
	if err != nil {
		t.Fatalf("acknowledge event: %v", err)
	}

	merged, err := svc.Create(ctx, faultReportInput("FE-201", now.Add(3*time.Minute)), "operator", "req-3")
	if err != nil {
		t.Fatalf("create duplicate report: %v", err)
	}
	if merged.ID != acknowledged.ID {
		t.Fatalf("expected merge into acknowledged event %d, got %d", acknowledged.ID, merged.ID)
	}
	if merged.Status != "open" {
		t.Fatalf("expected acknowledged event to fall back to open, got %s", merged.Status)
	}
	if merged.OccurrenceCount != 2 {
		t.Fatalf("expected occurrence=2, got %d", merged.OccurrenceCount)
	}
}

func TestFaultEventMitigatedAndClosedArePreserved(t *testing.T) {
	svc, _ := newFaultEventTestService(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first, err := svc.Create(ctx, faultReportInput("FE-300", now), "operator", "req-1")
	if err != nil {
		t.Fatalf("create first report: %v", err)
	}
	mitigated, err := svc.Transition(ctx, first.ID, dto.TransitionRequest{
		Status: "mitigated", ExpectedVersion: first.Version, Reason: "已远程降载",
	}, "operator", "req-2")
	if err != nil {
		t.Fatalf("mitigate event: %v", err)
	}

	reported, err := svc.Create(ctx, faultReportInput("FE-301", now.Add(2*time.Minute)), "operator", "req-3")
	if err != nil {
		t.Fatalf("create report against mitigated event: %v", err)
	}
	if reported.ID == mitigated.ID {
		t.Fatalf("expected mitigated event to be preserved, got merged record %d", reported.ID)
	}
	if reported.OccurrenceCount != 1 || reported.Status != "open" {
		t.Fatalf("expected a fresh open event, got %+v", reported)
	}
	if total := countFaultEvents(t, svc); total != 2 {
		t.Fatalf("expected mitigated record plus new event, got %d records", total)
	}
}

func TestFaultEventOutsideWindowCreatesNewRecord(t *testing.T) {
	svc, _ := newFaultEventTestService(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first, err := svc.Create(ctx, faultReportInput("FE-400", now.Add(-30*time.Minute)), "operator", "req-1")
	if err != nil {
		t.Fatalf("create first report: %v", err)
	}
	second, err := svc.Create(ctx, faultReportInput("FE-401", now), "operator", "req-2")
	if err != nil {
		t.Fatalf("create report outside window: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("expected report 30 minutes later to create a new event, merged into %d", first.ID)
	}
	if total := countFaultEvents(t, svc); total != 2 {
		t.Fatalf("expected two events outside the dedup window, got %d", total)
	}
}

func TestFaultEventMergeKeepsHigherRisk(t *testing.T) {
	svc, _ := newFaultEventTestService(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := svc.Create(ctx, faultReportInput("FE-500", now), "operator", "req-1"); err != nil {
		t.Fatalf("create first report: %v", err)
	}
	lower := faultReportInput("FE-501", now.Add(time.Minute))
	lower.RiskLevel = "low"
	merged, err := svc.Create(ctx, lower, "operator", "req-2")
	if err != nil {
		t.Fatalf("create lower-risk duplicate: %v", err)
	}
	if merged.RiskLevel != "medium" {
		t.Fatalf("expected risk to stay at medium, got %s", merged.RiskLevel)
	}
}

func TestMergeFaultEvidenceHelpers(t *testing.T) {
	if got := mergeFaultEvidence("", " 探头读数 "); got != "探头读数" {
		t.Fatalf("expected trimmed incoming evidence, got %q", got)
	}
	if got := mergeFaultEvidence("已记录", "已记录"); got != "已记录" {
		t.Fatalf("expected identical evidence to stay untouched, got %q", got)
	}
	if got := mergeFaultEvidence("第一次上报", "第二次上报"); got != "第一次上报 | 第二次上报" {
		t.Fatalf("expected appended evidence, got %q", got)
	}
	long := strings.Repeat("证", faultEvidenceLimit)
	if got := mergeFaultEvidence(long, "额外"); len([]rune(got)) != faultEvidenceLimit {
		t.Fatalf("expected evidence capped at %d runes, got %d", faultEvidenceLimit, len([]rune(got)))
	}
	if higherRiskLevel("critical", "low") != "critical" || higherRiskLevel("low", "high") != "high" {
		t.Fatal("expected higherRiskLevel to keep the more severe level")
	}
}
