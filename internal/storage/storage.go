package storage

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type Storage struct {
	db *gorm.DB
}

type RemoteSession struct {
	ID         string     `gorm:"primaryKey;type:text" json:"id"`
	StartTime  time.Time  `gorm:"not null;index" json:"start_time"`
	EndTime    *time.Time `gorm:"index" json:"end_time"`        // 使用指针代替 sql.NullTime
	Duration   int64      `gorm:"type:integer" json:"duration"` // 存储秒数
	Signals    string     `gorm:"type:text" json:"signals"`
	Confidence float64    `gorm:"type:real" json:"confidence"`
	CreatedAt  time.Time  `gorm:"autoCreateTime" json:"created_at"`
}

// MarshalJSON 自定义 JSON 序列化，正确处理 time.Duration
func (rs RemoteSession) MarshalJSON() ([]byte, error) {
	type Alias RemoteSession
	return json.Marshal(&struct {
		EndTime  *string `json:"end_time"` // 转换为字符串指针，null 时返回 null
		Duration int64   `json:"duration"` // 已经是秒数
		*Alias
	}{
		EndTime: func() *string {
			if rs.EndTime != nil {
				t := rs.EndTime.Format(time.RFC3339)
				return &t
			}
			return nil
		}(),
		Duration: rs.Duration, // 已经是秒数
		Alias:    (*Alias)(&rs),
	})
}

type RawSignal struct {
	ID         string    `gorm:"primaryKey;type:text" json:"id"`
	SessionID  *string   `gorm:"type:text;index" json:"session_id"` // 使用指针代替 sql.NullString
	Type       string    `gorm:"type:text;not null" json:"type"`
	Name       string    `gorm:"type:text;not null" json:"name"`
	Confidence float64   `gorm:"type:real;not null" json:"confidence"`
	RawData    string    `gorm:"type:text" json:"raw_data"`
	DetectedAt time.Time `gorm:"autoCreateTime;index" json:"detected_at"`
}

type Config struct {
	ID        string    `gorm:"primaryKey;type:text" json:"id"`
	Key       string    `gorm:"type:text;uniqueIndex;not null" json:"key"`
	Value     string    `gorm:"type:text;not null" json:"value"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

func NewStorage(dbPath string) (*Storage, error) {
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	s := &Storage{db: db}
	if err := s.runMigrations(); err != nil {
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	return s, nil
}

func (s *Storage) runMigrations() error {
	m := gormigrate.New(s.db, gormigrate.DefaultOptions, []*gormigrate.Migration{
		{
			ID: "20240101000000",
			Migrate: func(tx *gorm.DB) error {
				// 创建表
				if err := tx.AutoMigrate(&RemoteSession{}); err != nil {
					return err
				}
				if err := tx.AutoMigrate(&RawSignal{}); err != nil {
					return err
				}
				if err := tx.AutoMigrate(&Config{}); err != nil {
					return err
				}
				return nil
			},
			Rollback: func(tx *gorm.DB) error {
				// 回滚时删除表
				return tx.Migrator().DropTable(&RemoteSession{}, &RawSignal{}, &Config{})
			},
		},
	})

	return m.Migrate()
}

func (s *Storage) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func (s *Storage) SaveSession(session *RemoteSession) error {
	if session.ID == "" {
		session.ID = uuid.New().String()
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now()
	}

	return s.db.Create(session).Error
}

func (s *Storage) UpdateSessionEnd(sessionID string, endTime time.Time, duration time.Duration) error {
	return s.db.Model(&RemoteSession{}).
		Where("id = ?", sessionID).
		Updates(map[string]interface{}{
			"end_time": endTime,
			"duration": int64(duration.Seconds()),
		}).Error
}

func (s *Storage) GetOpenSession() (*RemoteSession, error) {
	var session RemoteSession
	err := s.db.Where("end_time IS NULL").First(&session).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &session, nil
}

func (s *Storage) GetRecentSessions(limit int) ([]RemoteSession, error) {
	var sessions []RemoteSession
	err := s.db.Order("start_time DESC").Limit(limit).Find(&sessions).Error
	return sessions, err
}

// GetSessionsPaginated 分页获取会话记录
// page: 页码（从1开始）
// pageSize: 每页数量
// 返回: 会话列表和总记录数
func (s *Storage) GetSessionsPaginated(page, pageSize int) ([]RemoteSession, int64, error) {
	var sessions []RemoteSession
	var total int64

	// 计算偏移量
	offset := (page - 1) * pageSize

	// 获取总数
	err := s.db.Model(&RemoteSession{}).Count(&total).Error
	if err != nil {
		return nil, 0, err
	}

	// 获取分页数据
	err = s.db.Order("start_time DESC").
		Offset(offset).
		Limit(pageSize).
		Find(&sessions).Error

	return sessions, total, err
}

func (s *Storage) SaveRawSignal(signal *RawSignal) error {
	if signal.ID == "" {
		signal.ID = uuid.New().String()
	}
	if signal.DetectedAt.IsZero() {
		signal.DetectedAt = time.Now()
	}

	return s.db.Create(signal).Error
}

func (s *Storage) GetConfig(key string) (string, error) {
	var config Config
	err := s.db.Where("key = ?", key).First(&config).Error
	if err == gorm.ErrRecordNotFound {
		return "", nil
	}
	return config.Value, err
}

func (s *Storage) SetConfig(key, value string) error {
	var existingConfig Config
	err := s.db.Where("key = ?", key).First(&existingConfig).Error

	if err == gorm.ErrRecordNotFound {
		// 记录不存在，创建新记录
		config := Config{
			ID:    uuid.New().String(),
			Key:   key,
			Value: value,
		}
		return s.db.Create(&config).Error
	} else if err != nil {
		// 其他错误
		return err
	}

	// 记录存在，更新它
	return s.db.Model(&existingConfig).
		Updates(map[string]interface{}{
			"value":      value,
			"updated_at": time.Now(),
		}).Error
}
