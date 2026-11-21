package store

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// User 登录用户。
type User struct {
	ID        string    `gorm:"primaryKey;size:36" json:"id"`
	Email     string    `gorm:"uniqueIndex;size:190" json:"email"`
	Name      string    `gorm:"size:120" json:"name"`
	Avatar    string    `gorm:"size:500" json:"avatar"`
	Role      string    `gorm:"size:16;default:user" json:"role"` // admin | user
	CreatedAt time.Time `json:"created_at"`
}

// Device 一个可管理的 Docker Engine。
type Device struct {
	ID        string    `gorm:"primaryKey;size:36" json:"id"`
	Name      string    `gorm:"size:120;uniqueIndex" json:"name"`
	Endpoint  string    `gorm:"size:500" json:"endpoint"`
	TLS       bool      `json:"tls"`
	CAPath    string    `gorm:"size:500" json:"-"`
	CertPath  string    `gorm:"size:500" json:"-"`
	KeyPath   string    `gorm:"size:500" json:"-"`
	DataDir   string    `gorm:"size:500" json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// Instance 一个被托管的服务实例（对应一个 Docker 容器）。
type Instance struct {
	ID          string    `gorm:"primaryKey;size:36" json:"id"`
	UserID      string    `gorm:"index;size:36" json:"user_id"`
	DeviceID    string    `gorm:"index;size:36" json:"device_id"`
	DeviceName  string    `gorm:"-" json:"device_name"`
	Name        string    `gorm:"size:120" json:"name"`
	Type        string    `gorm:"size:32" json:"type"` // redis | postgres | sqlite | custom
	Image       string    `gorm:"size:190" json:"image"`
	ContainerID string    `gorm:"size:80;index" json:"container_id"`
	Status      string    `gorm:"size:32;default:creating" json:"status"`
	HostPort    int       `json:"host_port"`
	InnerPort   int       `json:"inner_port"`
	EnvJSON     string    `gorm:"type:text" json:"-"`     // 凭据等环境变量（JSON）
	Notes       string    `gorm:"type:text" json:"notes"` // 连接串提示
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (Instance) TableName() string { return "instances" }

func NewID() string { return uuid.NewString() }

// Open 打开数据库。dbURL 为空时使用纯 Go SQLite（无需 CGO），
// 配置 postgres:// 时使用 PostgreSQL，满足"redis、pgsql、sqlite"多后端要求。
func Open(dbURL string) (*gorm.DB, error) {
	var (
		db  *gorm.DB
		err error
	)
	if dbURL == "" || hasPrefix(dbURL, "sqlite") {
		db, err = openSQLite(dbPath(dbURL))
	} else if hasPrefix(dbURL, "postgres") {
		db, err = openPostgres(dbURL)
	} else {
		return nil, errors.New("不支持的 HL_DB 格式，留空使用 SQLite 或填写 postgres:// 连接串")
	}
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&User{}, &Device{}, &Instance{}); err != nil {
		return nil, err
	}
	return db, nil
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func dbPath(dbURL string) string {
	// 支持 sqlite://file.db / sqlite:///abs/path.db / 空
	if dbURL == "" {
		return "data/harborlight.db"
	}
	p := dbURL
	for _, pre := range []string{"sqlite://", "sqlite:"} {
		if hasPrefix(p, pre) {
			p = p[len(pre):]
			break
		}
	}
	return p
}

// ErrNotFound 便捷错误。
var ErrNotFound = gorm.ErrRecordNotFound
