//go:build darwin && cgo

package platform

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codexrouter/internal/store"
)

func profileFixture(t *testing.T) (string, *darwinCredentials) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	legacyService, err := credentialServiceForProfile(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	fixture, cleanup, err := privateKeychain(filepath.Join(t.TempDir(), "profile.keychain-db"), legacyService)
	if err != nil {
		t.Fatal(err)
	}
	original := openProfileCredentials
	openProfileCredentials = func(service string) (Credentials, error) {
		return &darwinCredentials{service: service, keychain: fixture.keychain}, nil
	}
	t.Cleanup(func() { openProfileCredentials = original; cleanup() })
	return dir, fixture
}

func TestCredentialProfileMigrationPreservesLegacyAndInstanceState(t *testing.T) {
	dir, legacy := profileFixture(t)
	if err := legacy.Set("control", "synthetic-old-control"); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Set("company", "synthetic-old-company"); err != nil {
		t.Fatal(err)
	}
	snapshot := map[string][]byte{
		"state.json":      []byte(`{"version":2,"bindings":{"synthetic-thread":{"provider_id":"company"}}}`),
		"history-marker":  []byte("synthetic-history-untouched"),
		"recovery-marker": []byte("synthetic-daily-owner-unchanged"),
	}
	info := map[string]os.FileInfo{}
	for name, data := range snapshot {
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		info[name], _ = os.Stat(file)
	}
	running, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer running.Close()
	if err := PrepareCredentialProfile(dir); err != nil {
		t.Fatal(err)
	}
	state, err := CredentialProfiles(dir)
	if err != nil || state.Active || !state.Pending {
		t.Fatal("prepare selected new namespace", err)
	}
	current, err := credentialService(dir)
	if err != nil || current != legacy.service {
		t.Fatal("running/legacy selection changed", err)
	}
	pending, err := OpenPreparedCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"company", "unknown"} {
		if _, err := pending.Get(name); !errors.Is(err, ErrNotFound) {
			t.Fatal("old credential copied into new profile")
		}
	}
	if _, err := pending.Get("control"); err != nil {
		t.Fatal("new control unavailable", err)
	}
	record, err := os.ReadFile(filepath.Join(dir, profilePendingFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(record), "synthetic") || strings.Contains(string(record), "control") {
		t.Fatal("selector contains credential data")
	}
	if err := PrepareCredentialProfile(dir); err == nil {
		t.Fatal("pending profile replaced")
	}
	again, _ := os.ReadFile(filepath.Join(dir, profilePendingFile))
	if string(again) != string(record) {
		t.Fatal("pending profile mutated")
	}
	if err := ActivateCredentialProfile(dir); err == nil {
		t.Fatal("active instance lock ignored")
	}
	if err := running.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ActivateCredentialProfile(dir); err != nil {
		t.Fatal(err)
	}
	active, err := OpenCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := active.Get("company"); !errors.Is(err, ErrNotFound) {
		t.Fatal("active profile fell back to legacy")
	}
	if err := active.Set("company", "synthetic-reentered-company"); err != nil {
		t.Fatal(err)
	}
	if got, err := legacy.Get("company"); err != nil || got != "synthetic-old-company" {
		t.Fatal("legacy credential changed", err)
	}
	if err := PrepareCredentialProfile(dir); err == nil {
		t.Fatal("active selector replaced implicitly")
	}
	currentInfo, err := os.Stat(filepath.Join(dir, profileActiveFile))
	if err != nil || currentInfo.Mode().Perm() != 0600 {
		t.Fatal("selector permissions wrong", err)
	}
	lock, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := RollbackCredentialProfile(dir); err == nil {
		t.Fatal("rollback ignored running instance")
	}
	_ = lock.Close()
	if err := RollbackCredentialProfile(dir); err != nil {
		t.Fatal(err)
	}
	selected, err := OpenCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := selected.Get("company"); err != nil || got != "synthetic-old-company" {
		t.Fatal("legacy selection not restored", err)
	}
	if got, err := active.Get("company"); err != nil || got != "synthetic-reentered-company" {
		t.Fatal("rollback deleted new credential", err)
	}
	for name, data := range snapshot {
		file := filepath.Join(dir, name)
		actual, err := os.ReadFile(file)
		after, _ := os.Stat(file)
		if err != nil || string(actual) != string(data) || !os.SameFile(info[name], after) || info[name].Mode() != after.Mode() || !info[name].ModTime().Equal(after.ModTime()) {
			t.Fatal("migration modified routing/history/recovery file", name)
		}
	}
}

func TestCredentialProfileSelectorsFailClosed(t *testing.T) {
	dir, _ := profileFixture(t)
	file := filepath.Join(dir, profileActiveFile)
	for _, raw := range []string{`{`, `{"version":2,"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, `{"version":1,"id":"bad"}`, `{"version":1,"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","secret":"synthetic"}`, `{"version":1,"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {}`} {
		if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenCredentials(dir); err == nil {
			t.Fatal("bad selector silently fell back")
		}
	}
	valid := []byte(`{"version":1,"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	if err := os.WriteFile(file, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := credentialService(dir); err == nil {
		t.Fatal("public selector accepted")
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.Rename(file, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	if _, err := credentialService(dir); err == nil {
		t.Fatal("symlink selector accepted")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, file); err != nil {
		t.Fatal(err)
	}
	if _, err := credentialService(dir); err == nil {
		t.Fatal("hardlinked selector accepted")
	}
	one, err := credentialServiceForProfile(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	other, err := credentialServiceForProfile(filepath.Join(dir, "another"), "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil || one == other {
		t.Fatal("profile split lost directory isolation", err)
	}
}

type deniedProfileCredentials struct{ Credentials }

func (c *deniedProfileCredentials) Check(string) error { return &CredentialError{Status: -25293} }
func (c *deniedProfileCredentials) Authorize(string) error {
	return errors.New("unexpected interaction")
}
func TestCredentialProfilePrepareFailureDoesNotPublishOrTouchOld(t *testing.T) {
	dir, legacy := profileFixture(t)
	if err := legacy.Set("control", "synthetic-legacy-retained"); err != nil {
		t.Fatal(err)
	}
	factory := openProfileCredentials
	var fresh Credentials
	openProfileCredentials = func(service string) (Credentials, error) {
		if service == legacy.service {
			t.Fatal("prepare read old credentials")
		}
		var err error
		fresh, err = factory(service)
		if err != nil {
			return nil, err
		}
		return &deniedProfileCredentials{fresh}, nil
	}
	if err := PrepareCredentialProfile(dir); err == nil {
		t.Fatal("access denial hidden")
	}
	if fresh == nil {
		t.Fatal("new backend not used")
	}
	if _, err := fresh.Get("control"); !errors.Is(err, ErrNotFound) {
		t.Fatal("failed prepared credential not cleaned")
	}
	for _, name := range []string{profilePendingFile, profileActiveFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("failed migration published selector")
		}
	}
	if got, err := legacy.Get("control"); err != nil || got != "synthetic-legacy-retained" {
		t.Fatal("old control touched", err)
	}
}

func TestCredentialProfileActivationRecoversPublishedSelector(t *testing.T) {
	dir, _ := profileFixture(t)
	if err := PrepareCredentialProfile(dir); err != nil {
		t.Fatal(err)
	}
	pending, err := readCredentialProfile(dir, profilePendingFile)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate exit after publishing active selector but before removing the pending record.
	if err := publishProfile(dir, profileActiveFile, pending); err != nil {
		t.Fatal(err)
	}
	if err := ActivateCredentialProfile(dir); err != nil {
		t.Fatal("idempotent activation failed", err)
	}
	state, err := CredentialProfiles(dir)
	if err != nil || !state.Active || state.Pending {
		t.Fatal("activation recovery inconsistent", err)
	}
}
