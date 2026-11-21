package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// DBWrapper 数据访问封装。
type DBWrapper struct {
	gdb *gorm.DB
}

func Wrap(gdb *gorm.DB) *DBWrapper { return &DBWrapper{gdb: gdb} }

func (d *DBWrapper) Gorm() *gorm.DB { return d.gdb }

// UpsertUser 按 Email 幂等写入/更新用户。
func (d *DBWrapper) UpsertUser(ctx context.Context, u *User) (*User, error) {
	var existing User
	err := d.gdb.WithContext(ctx).Where("email = ?", u.Email).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		u.ID = NewID()
		u.CreatedAt = time.Now()
		if u.Role == "" {
			u.Role = "user"
		}
		if err := d.gdb.Create(u).Error; err != nil {
			return nil, err
		}
		return u, nil
	}
	if err != nil {
		return nil, err
	}
	// 资料更新时保留既有角色；仅由服务端明确传入 admin 时执行提权。
	updates := map[string]any{"name": u.Name, "avatar": u.Avatar}
	if u.Role == "admin" && existing.Role != "admin" {
		updates["role"] = "admin"
		existing.Role = "admin"
	}
	if err := d.gdb.Model(&existing).Updates(updates).Error; err != nil {
		return nil, err
	}
	existing.Name = u.Name
	existing.Avatar = u.Avatar
	return &existing, nil
}

func (d *DBWrapper) CreateDevice(device *Device) error {
	device.ID = NewID()
	device.CreatedAt = time.Now()
	return d.gdb.Create(device).Error
}

func (d *DBWrapper) ListDevices() ([]Device, error) {
	var out []Device
	err := d.gdb.Order("name ASC").Find(&out).Error
	return out, err
}

func (d *DBWrapper) GetDevice(id string) (*Device, error) {
	var device Device
	if err := d.gdb.Where("id = ?", id).First(&device).Error; err != nil {
		return nil, err
	}
	return &device, nil
}

func (d *DBWrapper) DeleteDevice(id string) error {
	return d.gdb.Where("id = ?", id).Delete(&Device{}).Error
}

func (d *DBWrapper) DeviceInstanceCount(id string) (int64, error) {
	var count int64
	err := d.gdb.Model(&Instance{}).Where("device_id = ?", id).Count(&count).Error
	return count, err
}

func (d *DBWrapper) CreateInstance(inst *Instance) error {
	inst.CreatedAt = time.Now()
	inst.UpdatedAt = time.Now()
	return d.gdb.Create(inst).Error
}

func (d *DBWrapper) ListInstances(userID string) ([]Instance, error) {
	var out []Instance
	err := d.gdb.Where("user_id = ?", userID).Order("created_at DESC").Find(&out).Error
	return out, err
}

func (d *DBWrapper) GetInstance(id string) (*Instance, error) {
	var inst Instance
	if err := d.gdb.Where("id = ?", id).First(&inst).Error; err != nil {
		return nil, err
	}
	return &inst, nil
}

func (d *DBWrapper) UpdateStatus(id, status string) error {
	return d.gdb.Model(&Instance{}).Where("id = ?", id).
		Updates(map[string]any{"status": status, "updated_at": time.Now()}).Error
}

func (d *DBWrapper) DeleteInstance(id string) error {
	return d.gdb.Where("id = ?", id).Delete(&Instance{}).Error
}
