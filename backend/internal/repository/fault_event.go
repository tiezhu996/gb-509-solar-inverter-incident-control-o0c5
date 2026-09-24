package repository

import (
	"context"
	"time"

	"github.com/blueship581/solar-inverter-incident-control/backend/internal/constants"
	"github.com/blueship581/solar-inverter-incident-control/backend/internal/dto"
	"github.com/blueship581/solar-inverter-incident-control/backend/internal/model"
	"gorm.io/gorm"
)

// FaultEventRepository owns all persistence operations for 故障事件.
type FaultEventRepository interface {
	List(context.Context, dto.PageQuery) (Page[model.FaultEvent], error)
	Get(context.Context, uint) (model.FaultEvent, error)
	Create(context.Context, *model.FaultEvent) error
	Update(context.Context, uint, uint, *model.FaultEvent) error
	Delete(context.Context, uint) error
	CountByStatus(context.Context) (map[string]int64, error)
	// FindRecentDuplicate returns the newest still-actionable event (open or
	// acknowledged) for the same site, related device and fault category whose
	// last report falls inside the dedup window. Mitigated and closed records
	// are deliberately preserved and never matched.
	FindRecentDuplicate(ctx context.Context, facility, relatedCode, category string, since time.Time) (model.FaultEvent, error)
}

type faultEventRepository struct {
	store *Store[model.FaultEvent]
}

func NewFaultEventRepository(db *gorm.DB) FaultEventRepository {
	return &faultEventRepository{store: NewStore[model.FaultEvent](db)}
}

func (r *faultEventRepository) List(ctx context.Context, q dto.PageQuery) (Page[model.FaultEvent], error) {
	return r.store.List(ctx, q)
}
func (r *faultEventRepository) Get(ctx context.Context, id uint) (model.FaultEvent, error) {
	return r.store.Get(ctx, id)
}
func (r *faultEventRepository) Create(ctx context.Context, item *model.FaultEvent) error {
	return r.store.Create(ctx, item)
}
func (r *faultEventRepository) Update(ctx context.Context, id, version uint, item *model.FaultEvent) error {
	return r.store.Update(ctx, id, version, item)
}
func (r *faultEventRepository) Delete(ctx context.Context, id uint) error {
	return r.store.Delete(ctx, id)
}
func (r *faultEventRepository) CountByStatus(ctx context.Context) (map[string]int64, error) {
	return r.store.CountByStatus(ctx)
}
func (r *faultEventRepository) FindRecentDuplicate(ctx context.Context, facility, relatedCode, category string, since time.Time) (model.FaultEvent, error) {
	var item model.FaultEvent
	result := r.store.db.WithContext(ctx).
		Where("facility = ? AND related_code = ? AND category = ?", facility, relatedCode, category).
		Where("status IN ?", []string{string(constants.FaultStateOpen), string(constants.FaultStateAcknowledged)}).
		Where("last_reported_at >= ?", since).
		Order("last_reported_at DESC, id DESC").
		Limit(1).
		Find(&item)
	if result.Error != nil {
		return item, result.Error
	}
	if result.RowsAffected == 0 {
		return item, gorm.ErrRecordNotFound
	}
	return item, nil
}
