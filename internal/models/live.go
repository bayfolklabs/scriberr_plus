package models

import "time"

// LiveSegment is one final segment produced by the live ASR worker during a live
// session. Times are absolute session seconds. Segments are materialized into the
// job's LiveTranscript (and Transcript) when the session is finalized.
type LiveSegment struct {
	ID                 uint      `json:"id" gorm:"primaryKey;autoIncrement"`
	TranscriptionJobID string    `json:"transcription_job_id" gorm:"type:varchar(36);not null;uniqueIndex:idx_live_segments_job_seq"`
	Seq                int       `json:"seq" gorm:"type:int;not null;uniqueIndex:idx_live_segments_job_seq"`
	StartSec           float64   `json:"start" gorm:"type:real"`
	EndSec             float64   `json:"end" gorm:"type:real"`
	Text               string    `json:"text" gorm:"type:text"`
	WordsJSON          *string   `json:"words_json,omitempty" gorm:"type:text"` // optional JSON [{word,start,end}]
	CreatedAt          time.Time `json:"created_at" gorm:"autoCreateTime"`
}

// TableName pins the table name for live segments
func (LiveSegment) TableName() string {
	return "live_segments"
}
