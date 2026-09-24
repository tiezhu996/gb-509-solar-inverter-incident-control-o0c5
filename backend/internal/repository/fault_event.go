package repository

import (
	"context"
	"time"

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
	// FindMergeCandidate returns the most recently reported event sharing the
	// facility + relatedCode + category dedup key whose last report falls
	// inside the merge window and whose status still absorbs duplicates.
	FindMergeCandidate(ctx context.Context, facility, relatedCode, category string, since time.Time, statuses []string) (model.FaultEvent, error)
}

type faultEventRepository struct {
	store *Store[model.FaultEvent]
	db    *gorm.DB
}

func NewFaultEventRepository(db *gorm.DB) FaultEventRepository {
	return &faultEventRepository{store: NewStore[model.FaultEvent](db), db: db}
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
func (r *faultEventRepository) FindMergeCandidate(ctx context.Context, facility, relatedCode, category string, since time.Time, statuses []string) (model.FaultEvent, error) {
	var item model.FaultEvent
	err := r.db.WithContext(ctx).
		Where("facility = ? AND related_code = ? AND category = ?", facility, relatedCode, category).
		Where("status IN ?", statuses).
		Where("last_reported_at >= ?", since).
		Order("last_reported_at DESC, id DESC").
		First(&item).Error
	return item, err
}
