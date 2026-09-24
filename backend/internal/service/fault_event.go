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

// faultDedupWindow is the sliding window in which repeated reports of the same
// fault (same site, related device and category) are merged into one event
// instead of creating a new record each time.
const faultDedupWindow = 15 * time.Minute

// faultEvidenceLimit mirrors the Evidence column size so merged evidence never
// exceeds what persistence can store.
const faultEvidenceLimit = 2000

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
	facility := strings.TrimSpace(input.Facility)
	category := strings.TrimSpace(input.Category)
	relatedCode := strings.ToUpper(strings.TrimSpace(input.RelatedCode))
	reportedAt := input.EffectiveAt.UTC()
	if reportedAt.IsZero() {
		reportedAt = time.Now().UTC()
	}
	merged, found, err := s.mergeDuplicateReport(ctx, facility, relatedCode, category, reportedAt, input, actor, requestID)
	if err != nil {
		return model.FaultEvent{}, err
	}
	if found {
		return merged, nil
	}
	item := model.FaultEvent{
		BaseModel: model.BaseModel{
			Code: strings.ToUpper(strings.TrimSpace(input.Code)), Name: strings.TrimSpace(input.Name),
			Status: model.FaultEventInitialStatus, Version: 1, Description: strings.TrimSpace(input.Description),
		},
		Facility: facility, Owner: strings.TrimSpace(input.Owner),
		Category: category, RiskLevel: input.RiskLevel,
		MetricValue: input.MetricValue, MetricUnit: strings.TrimSpace(input.MetricUnit),
		EffectiveAt: reportedAt, Evidence: strings.TrimSpace(input.Evidence),
		RelatedCode: relatedCode, OccurrenceCount: 1, LastReportedAt: reportedAt,
	}
	if err := s.repository.Create(ctx, &item); err != nil {
		return model.FaultEvent{}, fmt.Errorf("create 故障事件: %w", err)
	}
	_ = s.security.Audit(ctx, actor, requestID, "create", "FaultEvent", item.ID, "", item.Status, "created 故障事件")
	return item, nil
}

// mergeDuplicateReport folds a repeated report into the still-actionable event
// for the same site, device and fault type: the occurrence count grows, the
// last-report time advances, new evidence is appended and the risk level keeps
// the higher severity. An acknowledged event falls back to open so the repeat
// is handled again; mitigated or closed records are left untouched and simply
// never match the dedup query.
func (s *faultEventService) mergeDuplicateReport(ctx context.Context, facility, relatedCode, category string, reportedAt time.Time, input dto.CreateFaultEvent, actor, requestID string) (model.FaultEvent, bool, error) {
	since := reportedAt.Add(-faultDedupWindow)
	for attempt := 0; attempt < 3; attempt++ {
		existing, err := s.repository.FindRecentDuplicate(ctx, facility, relatedCode, category, since)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.FaultEvent{}, false, nil
		}
		if err != nil {
			return model.FaultEvent{}, false, fmt.Errorf("find duplicate 故障事件: %w", err)
		}
		before := existing.Status
		expectedVersion := existing.Version
		existing.OccurrenceCount++
		lastReported := existing.LastReportedAt
		if lastReported.IsZero() {
			lastReported = existing.EffectiveAt
		}
		if reportedAt.After(lastReported) {
			lastReported = reportedAt
		}
		existing.LastReportedAt = lastReported.UTC()
		existing.Evidence = mergeFaultEvidence(existing.Evidence, input.Evidence)
		existing.RiskLevel = higherRiskLevel(existing.RiskLevel, input.RiskLevel)
		if existing.Status == string(constants.FaultStateAcknowledged) {
			existing.Status = string(constants.FaultStateOpen)
		}
		existing.Version = expectedVersion + 1
		existing.UpdatedAt = time.Now().UTC()
		if err := s.repository.Update(ctx, existing.ID, expectedVersion, &existing); err != nil {
			if errors.Is(err, repository.ErrVersionConflict) {
				continue
			}
			return model.FaultEvent{}, false, fmt.Errorf("merge 故障事件: %w", err)
		}
		detail := fmt.Sprintf("merged duplicate report, occurrence=%d", existing.OccurrenceCount)
		_ = s.security.Audit(ctx, actor, requestID, "merge", "FaultEvent", existing.ID, before, existing.Status, detail)
		merged, err := s.repository.Get(ctx, existing.ID)
		if err != nil {
			return model.FaultEvent{}, false, err
		}
		return merged, true, nil
	}
	return model.FaultEvent{}, false, fmt.Errorf("merge 故障事件: %w", repository.ErrVersionConflict)
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

var faultRiskRank = map[string]int{"low": 1, "medium": 2, "high": 3, "critical": 4}

// higherRiskLevel keeps the more severe of two risk levels so a merged event
// never downgrades the measured risk.
func higherRiskLevel(current, incoming string) string {
	if faultRiskRank[incoming] > faultRiskRank[current] {
		return incoming
	}
	return current
}

// mergeFaultEvidence appends new evidence to the accumulated record, skipping
// empty or already-captured fragments and capping the result at the column
// limit without splitting multi-byte characters.
func mergeFaultEvidence(existing, incoming string) string {
	incoming = strings.TrimSpace(incoming)
	if incoming == "" {
		return existing
	}
	if existing == "" {
		return incoming
	}
	if existing == incoming || strings.Contains(existing, incoming) {
		return existing
	}
	merged := existing + " | " + incoming
	if runes := []rune(merged); len(runes) > faultEvidenceLimit {
		merged = string(runes[:faultEvidenceLimit])
	}
	return merged
}
