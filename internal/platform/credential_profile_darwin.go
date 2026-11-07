//go:build darwin

package platform

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

const profileActiveFile = "keychain-profile.json"
const profilePendingFile = "keychain-profile-pending.json"

var profileID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var openProfileCredentials = func(string) (Credentials, error) { return nil, ErrUnsupported }

type credentialProfile struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
}

// CredentialProfileState contains no secret values; absence of a selector keeps legacy behavior.
type CredentialProfileState struct {
	Active  bool
	Pending bool
}

func profileError() error {
	return errors.New("凭据空间记录不安全或格式无效；已停止，不会回退旧空间")
}

func readCredentialProfile(dir, name string) (*credentialProfile, error) {
	file := filepath.Join(dir, name)
	fd, err := syscall.Open(file, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, profileError()
	}
	f := os.NewFile(uintptr(fd), file)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, profileError()
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 1024 {
		return nil, profileError()
	}
	raw, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil || len(raw) > 1024 {
		return nil, profileError()
	}
	var profile credentialProfile
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&profile) != nil || d.Decode(new(any)) != io.EOF || profile.Version != 1 || !profileID.MatchString(profile.ID) {
		return nil, profileError()
	}
	return &profile, nil
}

func profileDirectory(data string) (string, error) {
	dir, err := DataDirectoryIdentity(data)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", errors.New("凭据迁移要求已存在的私有数据目录")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || !info.IsDir() || info.Mode().Perm() != 0700 {
		return "", errors.New("凭据迁移要求当前用户所有的0700数据目录")
	}
	return dir, nil
}

// Local flock coordinates profile commands; .instance.lock is also acquired for activation/rollback.
func lockProfileFile(dir, name string) (func(), error) {
	fd, err := syscall.Open(filepath.Join(dir, name), syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, profileError()
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, profileError()
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		f.Close()
		return nil, profileError()
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("实例或凭据迁移正在运行；请先正常退出目标路由器")
	}
	return func() { _ = f.Close() }, nil
}
func syncProfileDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// publishProfile writes a complete selector without overwriting any existing selector.
func publishProfile(dir, name string, profile *credentialProfile) error {
	raw, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".keychain-profile-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(raw, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err != nil {
		return err
	}
	if closed != nil {
		return closed
	}
	if err = os.Link(temp, filepath.Join(dir, name)); err != nil {
		return errors.New("已有凭据空间记录，未覆盖")
	}
	// Remove the temporary hard link before readers enforce link count == 1.
	if err = os.Remove(temp); err != nil {
		return err
	}
	return syncProfileDir(dir)
}

func CredentialProfiles(data string) (CredentialProfileState, error) {
	var result CredentialProfileState
	dir, err := profileDirectory(data)
	if err != nil {
		return result, err
	}
	active, err := readCredentialProfile(dir, profileActiveFile)
	if err != nil {
		return result, err
	}
	pending, err := readCredentialProfile(dir, profilePendingFile)
	if err != nil {
		return result, err
	}
	result.Active = active != nil
	result.Pending = pending != nil
	return result, nil
}

// OpenPreparedCredentials opens only the explicitly prepared new namespace, never the active/legacy one.
func OpenPreparedCredentials(data string) (Credentials, error) {
	dir, err := profileDirectory(data)
	if err != nil {
		return nil, err
	}
	pending, err := readCredentialProfile(dir, profilePendingFile)
	if err != nil {
		return nil, err
	}
	if pending == nil {
		return nil, errors.New("尚未准备新凭据空间")
	}
	service, err := credentialServiceForProfile(dir, pending.ID)
	if err != nil {
		return nil, err
	}
	return openProfileCredentials(service)
}

// PrepareCredentialProfile never reads/deletes old credentials and does not change the active selector.
// First migration only: an existing active profile must be reviewed, never silently replaced.
func PrepareCredentialProfile(data string) error {
	dir, err := profileDirectory(data)
	if err != nil {
		return err
	}
	unlock, err := lockProfileFile(dir, ".keychain-profile.lock")
	if err != nil {
		return err
	}
	defer unlock()
	state, err := CredentialProfiles(dir)
	if err != nil {
		return err
	}
	if state.Active || state.Pending {
		return errors.New("已有当前或待激活凭据空间；未覆盖，请先检查")
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	profile := &credentialProfile{Version: 1, ID: hex.EncodeToString(random[:])}
	service, err := credentialServiceForProfile(dir, profile.ID)
	if err != nil {
		return err
	}
	creds, err := openProfileCredentials(service)
	if err != nil {
		return err
	}
	if _, err := creds.Get("control"); !errors.Is(err, ErrNotFound) {
		return errors.New("新空间无法确认为空；未写入")
	}
	var token [32]byte
	if _, err = rand.Read(token[:]); err != nil {
		return err
	}
	if err = creds.Set("control", hex.EncodeToString(token[:])); err != nil {
		return err
	}
	access, ok := creds.(CredentialAccess)
	if !ok {
		_ = creds.Delete("control")
		return ErrUnsupported
	}
	if err = access.Check("control"); err != nil {
		_ = creds.Delete("control")
		return err
	}
	if err = publishProfile(dir, profilePendingFile, profile); err != nil {
		// An fsync error can occur after the complete record is published. Retain its credential.
		if saved, readErr := readCredentialProfile(dir, profilePendingFile); readErr == nil && saved != nil && saved.ID == profile.ID {
			return fmt.Errorf("新空间已保留但记录同步未确认，请检查后继续: %w", err)
		}
		if cleanup := creds.Delete("control"); cleanup != nil {
			return errors.New("准备失败且新空间控制条目清理未确认；旧条目未修改")
		}
		return err
	}
	return nil
}

// ActivateCredentialProfile requires the router's actual instance lock before changing the selector.
func ActivateCredentialProfile(data string) error {
	dir, err := profileDirectory(data)
	if err != nil {
		return err
	}
	instance, err := lockProfileFile(dir, ".instance.lock")
	if err != nil {
		return err
	}
	defer instance()
	unlock, err := lockProfileFile(dir, ".keychain-profile.lock")
	if err != nil {
		return err
	}
	defer unlock()
	active, err := readCredentialProfile(dir, profileActiveFile)
	if err != nil {
		return err
	}
	pending, err := readCredentialProfile(dir, profilePendingFile)
	if err != nil {
		return err
	}
	if pending == nil {
		return errors.New("尚未准备新凭据空间")
	}
	if active != nil && active.ID != pending.ID {
		return errors.New("当前凭据空间已改变，未覆盖")
	}
	creds, err := OpenPreparedCredentials(dir)
	if err != nil {
		return err
	}
	access, ok := creds.(CredentialAccess)
	if !ok {
		return ErrUnsupported
	}
	if err = access.Check("control"); err != nil {
		return err
	}
	if active == nil {
		if err = publishProfile(dir, profileActiveFile, pending); err != nil {
			return err
		}
	}
	if err = os.Remove(filepath.Join(dir, profilePendingFile)); err != nil {
		return err
	}
	return syncProfileDir(dir)
}

// RollbackCredentialProfile preserves new and old entries, returning selection to the legacy namespace.
func RollbackCredentialProfile(data string) error {
	dir, err := profileDirectory(data)
	if err != nil {
		return err
	}
	instance, err := lockProfileFile(dir, ".instance.lock")
	if err != nil {
		return err
	}
	defer instance()
	unlock, err := lockProfileFile(dir, ".keychain-profile.lock")
	if err != nil {
		return err
	}
	defer unlock()
	active, err := readCredentialProfile(dir, profileActiveFile)
	if err != nil {
		return err
	}
	if active == nil {
		return errors.New("当前已使用原凭据空间，未更改")
	}
	retired := "keychain-profile-retired-" + active.ID + ".json"
	if _, err = os.Lstat(filepath.Join(dir, retired)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("已有保留记录，未覆盖")
	}
	if err = os.Rename(filepath.Join(dir, profileActiveFile), filepath.Join(dir, retired)); err != nil {
		return err
	}
	return syncProfileDir(dir)
}
