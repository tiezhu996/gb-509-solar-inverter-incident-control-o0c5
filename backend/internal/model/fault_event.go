package model

import "time"

// FaultEvent models 故障事件 as an independently versioned aggregate. The fields
// cover ownership, operational context, evidence and measured risk so later
// changes naturally span persistence, service and UI layers.
type FaultEvent struct {
	BaseModel
	Facility    string    `json:"facility" gorm:"size:120;index"`
	Owner       string    `json:"owner" gorm:"size:120;index"`
	Category    string    `json:"category" gorm:"size:80;index"`
	RiskLevel   string    `json:"riskLevel" gorm:"size:32;index"`
	MetricValue float64   `json:"metricValue"`
	MetricUnit  string    `json:"metricUnit" gorm:"size:24"`
	EffectiveAt time.Time `json:"effectiveAt"`
	Evidence    string    `json:"evidence" gorm:"size:2000"`
	RelatedCode string    `json:"relatedCode" gorm:"size:64;index"`
	// OccurrenceCount tracks how many duplicate reports were folded into this
	// event, and LastReportedAt is the most recent report time. Repeated
	// reports for the same facility + relatedCode + category inside the merge
	// window update these fields instead of creating new events.
	OccurrenceCount int       `json:"occurrenceCount" gorm:"not null;default:1"`
	LastReportedAt  time.Time `json:"lastReportedAt" gorm:"index"`
}

func (item *FaultEvent) GetBase() *BaseModel { return &item.BaseModel }

func (item FaultEvent) TableName() string { return "fault_events" }

var FaultEventInitialStatus = "open"
