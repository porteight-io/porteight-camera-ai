package db

import (
	"errors"
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

func Init(path string) error {
	var err error
	DB, err = gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return err
	}

	err = DB.AutoMigrate(&User{}, &StreamKey{}, &Recording{})
	if err != nil {
		return err
	}

	return seedAdminUser()
}

func seedAdminUser() error {
	var count int64
	DB.Model(&User{}).Count(&count)
	if count == 0 {
		// Create default admin user
		// user: admin, pass: admin123
		return CreateUser("admin", "admin123")
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
