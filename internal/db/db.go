package db

import (
	"errors"
	"os"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var DB *gorm.DB

type User struct {
	ID       uint   `gorm:"primaryKey" json:"id"`
	Username string `gorm:"uniqueIndex" json:"username"`
	Password string `json:"-"` // Store hash, do not expose in JSON
}

type StreamKey struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Key         string    `gorm:"uniqueIndex" json:"key"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

type Recording struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	StreamKey string    `json:"stream_key"`
	FilePath  string    `json:"file_path"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	Duration  int64     `json:"duration"` // in seconds
}

// HLSRecordingSession indexes a completed (or in-progress) archived HLS session.
// This allows listing/serving recordings even when files live in S3.
type HLSRecordingSession struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	StreamKey string `gorm:"index:idx_stream_session,unique" json:"streamKey"`
	Session   string `gorm:"index:idx_stream_session,unique" json:"session"` // folder name: 2006-01-02_15-04-05

	// Storage
	Storage   string `json:"storage"` // "local" | "s3"
	LocalDir  string `json:"localDir"`
	S3Bucket  string `json:"s3Bucket"`
	S3Prefix  string `json:"s3Prefix"` // e.g. recordings/<key>/<session>/
	Uploaded  bool   `json:"uploaded"`
	UploadErr string `json:"uploadErr"`

	// Metadata
	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime"`
	Duration  float64   `json:"duration"` // seconds
	SizeBytes int64     `json:"sizeBytes"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// CameraAccessUser stores the explicit allowlist for camera-service access.
// A valid frontend auth token is still required; this table is the second gate.
type CameraAccessUser struct {
	ID              uint      `gorm:"primaryKey" json:"id"`
	SuvidhiUserID   string    `gorm:"uniqueIndex" json:"suvidhiUserId"`
	Email           string    `json:"email"`
	Name            string    `json:"name"`
	Enabled         bool      `gorm:"default:true" json:"enabled"`
	Notes           string    `json:"notes"`
	LastValidatedAt time.Time `json:"lastValidatedAt"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

func Init(path string) error {
	var err error
	DB, err = gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return err
	}

	err = DB.AutoMigrate(&User{}, &StreamKey{}, &Recording{}, &HLSRecordingSession{}, &CameraAccessUser{})
	if err != nil {
		return err
	}

	return seedAdminUser()
}

func UpsertHLSRecordingSession(s *HLSRecordingSession) error {
	if s == nil {
		return errors.New("nil session")
	}
	var existing HLSRecordingSession
	err := DB.Where("stream_key = ? AND session = ?", s.StreamKey, s.Session).First(&existing).Error
	if err == nil {
		s.ID = existing.ID
		return DB.Save(s).Error
	}
	return DB.Create(s).Error
}

func MarkHLSRecordingUploaded(streamKey, session string, uploaded bool, uploadErr string, storage string, s3Bucket string, s3Prefix string) error {
	return DB.Model(&HLSRecordingSession{}).
		Where("stream_key = ? AND session = ?", streamKey, session).
		Updates(map[string]any{
			"uploaded":   uploaded,
			"upload_err": uploadErr,
			"storage":    storage,
			"s3_bucket":  s3Bucket,
			"s3_prefix":  s3Prefix,
		}).Error
}

func ListHLSRecordingSessions(streamKey string) ([]HLSRecordingSession, error) {
	var out []HLSRecordingSession
	err := DB.Where("stream_key = ?", streamKey).Order("start_time asc").Find(&out).Error
	return out, err
}

func UpsertCameraAccessUser(entry *CameraAccessUser) error {
	if entry == nil {
		return errors.New("nil camera access user")
	}
	if entry.SuvidhiUserID == "" {
		return errors.New("suvidhi user id is required")
	}

	var existing CameraAccessUser
	err := DB.Where("suvidhi_user_id = ?", entry.SuvidhiUserID).First(&existing).Error
	if err == nil {
		entry.ID = existing.ID
		return DB.Save(entry).Error
	}
	return DB.Create(entry).Error
}

func GetCameraAccessUser(suvidhiUserID string) (*CameraAccessUser, error) {
	if suvidhiUserID == "" {
		return nil, errors.New("suvidhi user id is required")
	}
	var entry CameraAccessUser
	if err := DB.Where("suvidhi_user_id = ?", suvidhiUserID).First(&entry).Error; err != nil {
		return nil, err
	}
	return &entry, nil
}

func TouchCameraAccessUserValidation(suvidhiUserID string) error {
	if suvidhiUserID == "" {
		return errors.New("suvidhi user id is required")
	}
	return DB.Model(&CameraAccessUser{}).
		Where("suvidhi_user_id = ?", suvidhiUserID).
		Update("last_validated_at", time.Now()).
		Error
}

func SetCameraAccessUserEnabled(suvidhiUserID string, enabled bool) error {
	if suvidhiUserID == "" {
		return errors.New("suvidhi user id is required")
	}
	return DB.Model(&CameraAccessUser{}).
		Where("suvidhi_user_id = ?", suvidhiUserID).
		Update("enabled", enabled).
		Error
}

func DeleteCameraAccessUser(suvidhiUserID string) error {
	if suvidhiUserID == "" {
		return errors.New("suvidhi user id is required")
	}
	return DB.Where("suvidhi_user_id = ?", suvidhiUserID).Delete(&CameraAccessUser{}).Error
}

func ListCameraAccessUsers() ([]CameraAccessUser, error) {
	var out []CameraAccessUser
	err := DB.Order("enabled desc, updated_at desc, created_at desc").Find(&out).Error
	return out, err
}

func seedAdminUser() error {
	username := os.Getenv("ADMIN_USERNAME")
	if username == "" {
		username = "admin"
	}
	password := os.Getenv("ADMIN_PASSWORD")
	if password == "" {
		password = "admin123"
	}

	var count int64
	DB.Model(&User{}).Count(&count)
	if count == 0 {
		return CreateUser(username, password)
	}
	return nil
}

func CreateUser(username, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	user := &User{Username: username, Password: string(hash)}
	return DB.Create(user).Error
}

func Authenticate(username, password string) (*User, error) {
	var user User
	if err := DB.Where("username = ?", username).First(&user).Error; err != nil {
		return nil, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)); err != nil {
		return nil, errors.New("invalid password")
	}
	return &user, nil
}

func CreateStreamKey(key, desc string) (*StreamKey, error) {
	sk := &StreamKey{Key: key, Description: desc, CreatedAt: time.Now()}
	result := DB.Create(sk)
	if result.Error != nil {
		return nil, result.Error
	}
	return sk, nil
}

func GetStreamKey(key string) (*StreamKey, error) {
	var sk StreamKey
	result := DB.Where("key = ?", key).First(&sk)
	if result.Error != nil {
		return nil, result.Error
	}
	return &sk, nil
}

func GetAllStreamKeys() ([]StreamKey, error) {
	var keys []StreamKey
	result := DB.Find(&keys)
	return keys, result.Error
}

func SaveRecording(rec *Recording) error {
	return DB.Create(rec).Error
}

func GetAllRecordings() ([]Recording, error) {
	var recs []Recording
	result := DB.Order("start_time desc").Find(&recs)
	return recs, result.Error
}
