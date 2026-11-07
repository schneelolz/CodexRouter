//go:build darwin

package daily

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// Explicit opt-in: this reads only config.toml and path metadata, never auth or history.
// Do not invoke Status/Preview here: their database path can create lock/recovery files.
func TestDarwinDailyReadOnlyPreflight(t *testing.T) {
	if os.Getenv("CODEXROUTER_DAILY_READONLY") != "1" {
		t.Skip("explicit opt-in for real config read-only preflight")
	}
	owner := filepath.Join(t.TempDir(), "synthetic-owner")
	m, err := Discover("http://127.0.0.1:19973", owner)
	if err != nil {
		t.Fatal("cannot safely discover daily config")
	}
	t.Logf("config_path=%s", m.config)
	for _, key := range []string{"CODEX_APP_SERVER_OPENAI_BASE_URL", "CODEX_APP_SERVER_WS_URL", "CODEX_APP_SERVER_FORCE_CLI", "CODEX_CLI_PATH", "CODEX_APP_RUNTIME_CLI_PATH"} {
		t.Logf("environment_override_%s_present=%t", key, os.Getenv(key) != "")
	}
	if err := safePath(m.home, true); err != nil {
		t.Fatal("daily directory path is unsupported; nothing modified")
	}
	if err := safePath(m.config, false); err != nil {
		t.Fatal("daily config path is unsupported; nothing modified")
	}
	beforeInfo, err := os.Stat(m.config)
	if err != nil || beforeInfo.Size() > 1<<20 {
		t.Fatal("daily config unreadable or exceeds limit; nothing modified")
	}
	before, err := os.ReadFile(m.config)
	if err != nil {
		t.Fatal("daily config unreadable; nothing modified")
	}
	_, recoveryBefore := os.Lstat(m.dir)
	defer func() {
		after, readErr := os.ReadFile(m.config)
		afterInfo, statErr := os.Stat(m.config)
		if readErr != nil || statErr != nil || !bytes.Equal(before, after) || !os.SameFile(beforeInfo, afterInfo) || beforeInfo.Mode() != afterInfo.Mode() || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
			t.Error("configuration changed during preflight; no writes were issued by this test")
		} else {
			t.Log("config_bytes_identity_permissions_mtime_unchanged=true")
		}
		_, recoveryAfter := os.Lstat(m.dir)
		if os.IsNotExist(recoveryBefore) && !os.IsNotExist(recoveryAfter) {
			t.Error("recovery directory appeared during read-only preflight")
		}
	}()
	var doc map[string]any
	if err := toml.Unmarshal(before, &doc); err != nil {
		t.Fatal("daily TOML unsupported; contents withheld")
	}
	provider, providerPresent := doc["model_provider"]
	_, profile := doc["profile"]
	_, profiles := doc["profiles"]
	_, basePresent := doc["openai_base_url"]
	models, _ := doc["model_providers"].(map[string]any)
	_, openaiOverride := models["openai"]
	t.Logf("model_provider_present=%t builtin_openai_or_default=%t profile_present=%t profiles_present=%t custom_openai_definition=%t base_override_present=%t recovery_directory_present=%t", providerPresent, !providerPresent || provider == "openai", profile, profiles, openaiOverride, basePresent, recoveryBefore == nil)
	if m.blocked != "" {
		t.Fatal("DAILY_ENV_CONFLICT: process-level override blocks automatic daily integration; nothing modified")
	}
	snapshot, err := inspect(before)
	if err != nil {
		t.Fatal("DAILY_UNSUPPORTED: existing configuration is outside the supported daily editor; nothing modified")
	}
	if recoveryBefore == nil {
		t.Log("config_format_supported=true; existing recovery state requires separate ownership review; no lock acquired")
		return
	}
	if !os.IsNotExist(recoveryBefore) {
		t.Fatal("recovery path cannot be inspected safely")
	}
	next, _, err := m.prepare("route", before, snapshot, record{})
	if err != nil {
		t.Fatal("in-memory routing proposal rejected; nothing modified")
	}
	if _, err := inspect(next); err != nil {
		t.Fatal("in-memory routing proposal invalid; nothing modified")
	}
	t.Log("config_format_supported=true in_memory_routing_proposal_valid=true no_restore_state_created=true")
}
