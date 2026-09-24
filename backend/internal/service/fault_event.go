package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/blueship581/solar-inverter-incident-control/backend/internal/constants"
	"github.com/blueship581/solar-inverter-incident-control/backend/internal/dto"
	"github.com/blueship581/solar-inverter-incident-control/backend/internal/model"
	"github.com/blueship581/solar-inverter-incident-control/backend/internal/repository"
	"gorm.io/gorm"
)

// faultMergeWindow is how long repeated reports of the same fault fold into
// the surviving event instead of creating new ones.
const faultMergeWindow = 15 * time.Minute

// mergeableFaultStatuses are the states that still absorb duplicate reports;
// mitigated and closed records are preserved and never reopened by merging.
var mergeableFaultStatuses = []string{string(constants.FaultStateOpen), string(constants.FaultStateAcknowledged)}

type FaultEventService interface {
	List(context.Context, dto.PageQuery) (repository.Page[model.FaultEvent], error)
	Get(context.Context, uint) (model.FaultEvent, error)
	Create(context.Context, dto.CreateFaultEvent, string, string) (model.FaultEvent, error)
	Update(context.Context, uint, dto.UpdateFaultEvent, string, string) (model.FaultEvent, error)
	Transition(context.Context, uint, dto.TransitionRequest, string, string) (model.FaultEvent, error)
	Delete(context.Context, uint, string, string) error
	StatusCounts(context.Context) (map[string]int64, error)
}

type faultEventService struct {
	repository repository.FaultEventRepository
	security   SecurityService
}

func NewFaultEventService(repo repository.FaultEventRepository, security SecurityService) FaultEventService {
	return &faultEventService{repository: repo, security: security}
}

func (s *faultEventService) List(ctx context.Context, query dto.PageQuery) (repository.Page[model.FaultEvent], error) {
	return s.repository.List(ctx, query)
}

func (s *faultEventService) Get(ctx context.Context, id uint) (model.FaultEvent, error) {
	return s.repository.Get(ctx, id)
}

func (s *faultEventService) Create(ctx context.Context, input dto.CreateFaultEvent, actor, requestID string) (model.FaultEvent, error) {
	if err := validateFaultEventBusinessFields(input.Code, input.Name, input.Facility, input.Owner); err != nil {
		return model.FaultEvent{}, err
	}
	item := model.FaultEvent{
		BaseModel: model.BaseModel{
			Code: strings.ToUpper(strings.TrimSpace(input.Code)), Name: strings.TrimSpace(input.Name),
			Status: model.FaultEventInitialStatus, Version: 1, Description: strings.TrimSpace(input.Description),
		},
		Facility: strings.TrimSpace(input.Facility), Owner: strings.TrimSpace(input.Owner),
		Category: strings.TrimSpace(input.Category), RiskLevel: input.RiskLevel,
		MetricValue: input.MetricValue, MetricUnit: strings.TrimSpace(input.MetricUnit),
		EffectiveAt: input.EffectiveAt.UTC(), Evidence: strings.TrimSpace(input.Evidence),
		RelatedCode:     strings.ToUpper(strings.TrimSpace(input.RelatedCode)),
		OccurrenceCount: 1, LastReportedAt: input.EffectiveAt.UTC(),
	}
	if merged, ok, err := s.mergeDuplicateReport(ctx, &item, actor, requestID); err != nil {
		return model.FaultEvent{}, err
	} else if ok {
		return merged, nil
	}
	if err := s.repository.Create(ctx, &item); err != nil {
		return model.FaultEvent{}, fmt.Errorf("create 故障事件: %w", err)
	}
	_ = s.security.Audit(ctx, actor, requestID, "create", "FaultEvent", item.ID, "", item.Status, "created 故障事件")
	return item, nil
}

// mergeDuplicateReport folds a repeated report into the surviving event for
// the same facility + relatedCode + category inside the merge window. The
// survivor accumulates the occurrence count, latest report time, evidence and
// the higher risk level; an acknowledged event falls back to open so the
// recurrence is triaged again. It returns ok=false when no merge applies.
func (s *faultEventService) mergeDuplicateReport(ctx context.Context, incoming *model.FaultEvent, actor, requestID string) (model.FaultEvent, bool, error) {
	existing, err := s.repository.FindMergeCandidate(ctx, incoming.Facility, incoming.RelatedCode, incoming.Category,
		incoming.LastReportedAt.Add(-faultMergeWindow), mergeableFaultStatuses)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.FaultEvent{}, false, nil
	}
	if err != nil {
		return model.FaultEvent{}, false, fmt.Errorf("find duplicate 故障事件: %w", err)
	}
	if !withinMergeWindow(existing.LastReportedAt, incoming.LastReportedAt) {
		return model.FaultEvent{}, false, nil
	}
	before := existing.Status
	applyDuplicateReport(&existing, incoming)
	expectedVersion := existing.Version
	existing.Version = expectedVersion + 1
	existing.UpdatedAt = time.Now().UTC()
	if err := s.repository.Update(ctx, existing.ID, expectedVersion, &existing); err != nil {
		return model.FaultEvent{}, false, fmt.Errorf("merge duplicate 故障事件: %w", err)
	}
	detail := fmt.Sprintf("merged duplicate report, occurrence=%d", existing.OccurrenceCount)
	_ = s.security.Audit(ctx, actor, requestID, "merge", "FaultEvent", existing.ID, before, existing.Status, detail)
	merged, err := s.repository.Get(ctx, existing.ID)
	if err != nil {
		return model.FaultEvent{}, false, err
	}
	return merged, true, nil
}

// applyDuplicateReport mutates the surviving event with one repeated report.
func applyDuplicateReport(existing *model.FaultEvent, incoming *model.FaultEvent) {
	existing.OccurrenceCount++
	if incoming.LastReportedAt.After(existing.LastReportedAt) {
		existing.LastReportedAt = incoming.LastReportedAt
	}
	existing.Evidence = mergeEvidence(existing.Evidence, incoming.Evidence)
	existing.RiskLevel = higherRiskLevel(existing.RiskLevel, incoming.RiskLevel)
	if existing.Status == string(constants.FaultStateAcknowledged) {
		existing.Status = string(constants.FaultStateOpen)
	}
}

func withinMergeWindow(lastReportedAt, reportedAt time.Time) bool {
	gap := reportedAt.Sub(lastReportedAt)
	return gap <= faultMergeWindow && gap >= -faultMergeWindow
}

var faultRiskRank = map[string]int{"low": 1, "medium": 2, "high": 3, "critical": 4}

func higherRiskLevel(a, b string) string {
	if faultRiskRank[b] > faultRiskRank[a] {
		return b
	}
	return a
}

// mergeEvidence appends new evidence to the surviving record, skipping empty
// or already-captured fragments and respecting the column size limit.
func mergeEvidence(existing, incoming string) string {
	incoming = strings.TrimSpace(incoming)
	if incoming == "" || strings.Contains(existing, incoming) {
		return existing
	}
	if strings.TrimSpace(existing) == "" {
		return incoming
	}
	combined := existing + "；" + incoming
	const maxEvidenceRunes = 2000
	if runes := []rune(combined); len(runes) > maxEvidenceRunes {
		combined = string(runes[:maxEvidenceRunes])
	}
	return combined
}

func (s *faultEventService) Update(ctx context.Context, id uint, input dto.UpdateFaultEvent, actor, requestID string) (model.FaultEvent, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.FaultEvent{}, err
	}
	if err := validateFaultEventBusinessFields(current.Code, input.Name, input.Facility, input.Owner); err != nil {
		return model.FaultEvent{}, err
	}
	current.Name = strings.TrimSpace(input.Name)
	current.Description = strings.TrimSpace(input.Description)
	current.Facility = strings.TrimSpace(input.Facility)
	current.Owner = strings.TrimSpace(input.Owner)
	current.Category = strings.TrimSpace(input.Category)
	current.RiskLevel = input.RiskLevel
	current.MetricValue = input.MetricValue
	current.MetricUnit = strings.TrimSpace(input.MetricUnit)
	current.EffectiveAt = input.EffectiveAt.UTC()
	current.Evidence = strings.TrimSpace(input.Evidence)
	current.RelatedCode = strings.ToUpper(strings.TrimSpace(input.RelatedCode))
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = time.Now().UTC()
	if err := s.repository.Update(ctx, id, input.ExpectedVersion, &current); err != nil {
		return model.FaultEvent{}, fmt.Errorf("update 故障事件: %w", err)
	}
	_ = s.security.Audit(ctx, actor, requestID, "update", "FaultEvent", id, current.Status, current.Status, "updated business fields")
	return s.repository.Get(ctx, id)
}

func (s *faultEventService) Transition(ctx context.Context, id uint, input dto.TransitionRequest, actor, requestID string) (model.FaultEvent, error) {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return model.FaultEvent{}, err
	}
	target := strings.TrimSpace(input.Status)
	if !constants.CanTransition(constants.FaultEventTransitions, current.Status, target) {
		return model.FaultEvent{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current.Status, target)
	}
	before := current.Status
	current.Status = target
	current.Version = input.ExpectedVersion + 1
	current.UpdatedAt = time.Now().UTC()
	if err := s.repository.Update(ctx, id, input.ExpectedVersion, &current); err != nil {
		return model.FaultEvent{}, fmt.Errorf("transition 故障事件: %w", err)
	}
	if err := s.security.Audit(ctx, actor, requestID, "transition", "FaultEvent", id, before, target, input.Reason); err != nil {
		return model.FaultEvent{}, fmt.Errorf("persist transition audit: %w", err)
	}
	return s.repository.Get(ctx, id)
}

func (s *faultEventService) Delete(ctx context.Context, id uint, actor, requestID string) error {
	current, err := s.repository.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.repository.Delete(ctx, id); err != nil {
		return err
	}
	return s.security.Audit(ctx, actor, requestID, "delete", "FaultEvent", id, current.Status, "deleted", "soft deleted 故障事件")
}

func (s *faultEventService) StatusCounts(ctx context.Context) (map[string]int64, error) {
	return s.repository.CountByStatus(ctx)
}

func validateFaultEventBusinessFields(code, name, facility, owner string) error {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(facility) == "" || strings.TrimSpace(owner) == "" {
		return ErrInvalidInput
	}
	return nil
}
