package storage

import (
	"time"
)

func (s *RawSignal) GetSessionID() string {
	if s.SessionID != nil {
		return *s.SessionID
	}
	return ""
}

func (s *RawSignal) SetSessionID(id string) {
	if id == "" {
		s.SessionID = nil
	} else {
		s.SessionID = &id
	}
}

func (s *RemoteSession) GetEndTime() time.Time {
	if s.EndTime != nil {
		return *s.EndTime
	}
	return time.Time{}
}

func (s *RemoteSession) IsActive() bool {
	return s.EndTime == nil
}
