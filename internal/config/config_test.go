package config

import (
	"strings"
	"testing"
	"time"
)

func noEnv(string) (string, bool) { return "", false }

func TestDefaultsFollowTantei(t *testing.T) {
	cfg, err := Load(nil, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MAL.CycleDelay.Value() != 50*time.Second || cfg.MAL.RequestInterval.Value() != 3*time.Second {
		t.Fatalf("unexpected MAL defaults: %+v", cfg.MAL)
	}
	if cfg.AniList.CycleDelay.Value() != 90*time.Second || cfg.AniList.RequestsPerMinute != 29 {
		t.Fatalf("unexpected AniList defaults: %+v", cfg.AniList)
	}
	if !cfg.MAL.Enabled || !cfg.AniList.Enabled {
		t.Fatal("both providers should be enabled")
	}
	if cfg.Storage.Database != "data/kotantei.db" || cfg.Storage.TanteiDatabase != "data/data.db" {
		t.Fatalf("unexpected paths: %+v", cfg.Storage)
	}
}

func TestEnvironmentOverridesJSONSecretsAndPaths(t *testing.T) {
	env := map[string]string{
		"KOTANTEI_DISCORD_TOKEN":   "environment-token",
		"KOTANTEI_MAL_CLIENT_ID":   "environment-client",
		"KOTANTEI_DATABASE":        "custom/kotantei.db",
		"KOTANTEI_TANTEI_DATABASE": "custom/legacy.db",
	}
	cfg, err := Load(strings.NewReader(`{"discord":{"token":"file-token"},"mal":{"client_id":"file-client","cycle_delay":"2m"},"storage":{"database":"file/kotantei.db","tantei_database":"file/legacy.db"}}`), func(key string) (string, bool) { value, ok := env[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Discord.Token != env["KOTANTEI_DISCORD_TOKEN"] || cfg.MAL.ClientID != env["KOTANTEI_MAL_CLIENT_ID"] {
		t.Fatal("environment did not override file credentials")
	}
	if cfg.Storage.Database != env["KOTANTEI_DATABASE"] || cfg.Storage.TanteiDatabase != env["KOTANTEI_TANTEI_DATABASE"] {
		t.Fatal("environment did not override paths")
	}
	if cfg.MAL.CycleDelay.Value() != 2*time.Minute {
		t.Fatal("file duration not applied")
	}
}

func TestJSONCCommentsAndTrailingCommas(t *testing.T) {
	for _, input := range []string{
		`// Provider settings
{"mal":{"enabled":false}}`,
		`/* Leading comment */ {"mal": {"enabled": false /* inline comment */}} // End`,
		"{\r\n\"mal\": { // Windows newline\r\n\"enabled\": false\r\n}\r\n}",
		`{"mal": {"enabled": false,},}`,
		`{"mal": {"enabled": false, /* trailing comment */}, // Another comment
}`,
		`{/* Comment containing commas, braces } ], and "quotes" */ "mal":{"enabled":false}}`,
	} {
		t.Run(input, func(t *testing.T) {
			cfg, err := Load(strings.NewReader(input), noEnv)
			if err != nil {
				t.Fatal("valid JSONC configuration was rejected", err)
			}
			if cfg.MAL.Enabled || !cfg.AniList.Enabled {
				t.Fatal("JSONC settings or defaults were lost", cfg.MAL.Enabled, cfg.AniList.Enabled)
			}
		})
	}
}

func TestJSONCCommentsDoNotChangeQuotedValues(t *testing.T) {
	input := `{
		"project": {"source_url": "https://github.com/theinternetuse7/Kotantei//source/*literal*/"},
		"discord": {"token": "quoted \"// not a comment\" and \\ /* literal */"}, // Actual comment
	}`
	cfg, err := Load(strings.NewReader(input), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Project.SourceURL != "https://github.com/theinternetuse7/Kotantei//source/*literal*/" || cfg.Discord.Token != `quoted "// not a comment" and \ /* literal */` {
		t.Fatal("comment-like text inside strings was changed")
	}
}

func TestMalformedJSONCIsRejectedWithoutExposingValues(t *testing.T) {
	for _, input := range []string{
		`/* DO-NOT-PRINT`,
		`{"discord":{"token":"DO-NOT-PRINT"}, /* unfinished */`,
		`{"discord":{"token":"DO-NOT-PRINT}}`,
		`{"discord":{"token":"DO-NOT-PRINT"}} / bad`,
		`{"discord":{"token":"DO-NOT-PRINT"}, "mal":{"enabled":false,,}}`,
		`{,}`, `{"discord":{,}}`, `{"mal":{"enabled":,}}`,
		`{"mal":{"enabled":fal/* comment */se}}`,
		`# Not a JSONC comment
{"discord":{"token":"DO-NOT-PRINT"}}`,
	} {
		_, err := Load(strings.NewReader(input), noEnv)
		if err == nil || strings.Contains(err.Error(), "DO-NOT-PRINT") {
			t.Fatal("malformed JSONC was accepted or exposed its values", err)
		}
	}
}

func TestEnvironmentOverridesEveryProviderSetting(t *testing.T) {
	env := map[string]string{
		"KOTANTEI_ANILIST_ENABLED":             "true",
		"KOTANTEI_ANILIST_CYCLE_DELAY":         "2m",
		"KOTANTEI_ANILIST_REQUESTS_PER_MINUTE": "12",
		"KOTANTEI_MAL_ENABLED":                 "false",
		"KOTANTEI_MAL_TENRAI_ENABLED":          "true",
		"KOTANTEI_MAL_CYCLE_DELAY":             "75s",
		"KOTANTEI_MAL_REQUEST_INTERVAL":        "4s",
	}
	cfg, err := Load(strings.NewReader(`{"anilist":{"enabled":false},"mal":{"enabled":true}}`), func(key string) (string, bool) { value, ok := env[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AniList.Enabled || cfg.MAL.Enabled || !cfg.MAL.TenraiEnabled {
		t.Fatal("provider environment switches did not override JSON", cfg.AniList.Enabled, cfg.MAL.Enabled, cfg.MAL.TenraiEnabled)
	}
	if cfg.AniList.CycleDelay.Value() != 2*time.Minute || cfg.AniList.RequestsPerMinute != 12 || cfg.MAL.CycleDelay.Value() != 75*time.Second || cfg.MAL.RequestInterval.Value() != 4*time.Second {
		t.Fatal("provider environment limits were not applied", cfg.AniList, cfg.MAL)
	}
}

func TestEnvironmentConfiguresRuntimeWithoutJSON(t *testing.T) {
	env := map[string]string{
		"KOTANTEI_DISCORD_TOKEN":   "test-token",
		"KOTANTEI_MAL_ENABLED":     "false",
		"KOTANTEI_DATABASE":        "/data/kotantei.db",
		"KOTANTEI_LEGACY_DATABASE": "",
		"KOTANTEI_TANTEI_DATABASE": "/data/old.db",
	}
	cfg, err := Load(nil, func(key string) (string, bool) { value, ok := env[key]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateRuntime(); err != nil {
		t.Fatal("inline Compose settings could not configure an AniList-only runtime", err)
	}
	if cfg.Storage.Database != "/data/kotantei.db" || cfg.Storage.TanteiDatabase != "" {
		t.Fatal("generic legacy path did not override the compatibility alias", cfg.Storage)
	}
}

func TestInvalidEnvironmentSettingsAreRejectedWithoutExposingValues(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"KOTANTEI_ANILIST_ENABLED", "DO-NOT-PRINT"},
		{"KOTANTEI_MAL_ENABLED", "yes"},
		{"KOTANTEI_MAL_TENRAI_ENABLED", "1"},
		{"KOTANTEI_ANILIST_CYCLE_DELAY", "DO-NOT-PRINT"},
		{"KOTANTEI_MAL_CYCLE_DELAY", "0s"},
		{"KOTANTEI_MAL_REQUEST_INTERVAL", "-1s"},
		{"KOTANTEI_ANILIST_REQUESTS_PER_MINUTE", "DO-NOT-PRINT"},
		{"KOTANTEI_ANILIST_REQUESTS_PER_MINUTE", "0"},
		{"KOTANTEI_ANILIST_REQUESTS_PER_MINUTE", "30"},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			_, err := Load(nil, func(key string) (string, bool) { return tc.value, key == tc.key })
			if err == nil || !strings.Contains(err.Error(), tc.key) || strings.Contains(err.Error(), "DO-NOT-PRINT") {
				t.Fatal("invalid environment value was ignored or exposed", err)
			}
		})
	}
}

func TestInvalidConfigurationIsRejected(t *testing.T) {
	for _, input := range []string{
		`{"unknown":true}`, `{} {}`, `{"mal":{"cycle_delay":-1}}`,
		`{"mal":{"cycle_delay":"0s"}}`, `{"mal":{"request_interval":"-1s"}}`,
		`{"anilist":{"requests_per_minute":0}}`, `{"anilist":{"requests_per_minute":30}}`,
		`{"storage":{"database":" "}}`, `{"storage":{"database":"same.db","tantei_database":"same.db"}}`,
		`{"mal":{"enabled":null}}`, `null`,
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := Load(strings.NewReader(input), noEnv); err == nil {
				t.Fatal("expected invalid configuration error")
			}
		})
	}
}

func TestMaintenanceDoesNotRequireCredentials(t *testing.T) {
	if _, err := Load(nil, noEnv); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCredentialsAreRequiredOnlyForEnabledProviders(t *testing.T) {
	cfg, err := Load(nil, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateRuntime(); err == nil {
		t.Fatal("runtime accepted missing token")
	}
	cfg.Discord.Token = "test-token"
	if err := cfg.ValidateRuntime(); err == nil {
		t.Fatal("runtime accepted missing MAL client ID")
	}
	cfg.MAL.Enabled = false
	if err := cfg.ValidateRuntime(); err != nil {
		t.Fatal(err)
	}
	cfg.AniList.Enabled = false
	if err := cfg.ValidateRuntime(); err == nil {
		t.Fatal("runtime accepted no enabled providers")
	}
}

func TestValidationErrorsDoNotExposeSecrets(t *testing.T) {
	secret := "do-not-print-this-token"
	_, err := Load(strings.NewReader(`{"discord":{"token":"`+secret+`"},"mal":{"cycle_delay":"bad"}}`), noEnv)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestOversizedConfigurationIsRejected(t *testing.T) {
	input := `{"discord":{"token":"` + strings.Repeat("x", 1024*1024) + `"}}`
	if _, err := Load(strings.NewReader(input), noEnv); err == nil {
		t.Fatal("accepted oversized configuration")
	}
}

func TestTenraiEnrichmentIsOptIn(t *testing.T) {
	cfg, err := Load(nil, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MAL.TenraiEnabled {
		t.Fatal("third-party enrichment enabled by default")
	}
	cfg, err = Load(strings.NewReader(`{"mal":{"tenrai_enabled":true}}`), noEnv)
	if err != nil || !cfg.MAL.TenraiEnabled {
		t.Fatal("enrichment opt-in was not loaded", err)
	}
}

func TestRuntimeRejectsMalformedCredentialsWithoutExposingThem(t *testing.T) {
	for _, token := range []string{"Bearer DO-NOT-PRINT", "DO-NOT-PRINT\nheader", strings.Repeat("x", 513)} {
		cfg, err := Load(nil, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Discord.Token = token
		cfg.MAL.Enabled = false
		if err := cfg.ValidateRuntime(); err == nil || strings.Contains(err.Error(), "DO-NOT-PRINT") {
			t.Fatal("invalid Discord credential was accepted or exposed", err)
		}
	}
	for _, client := range []string{"DO-NOT-PRINT\nheader", " client ", strings.Repeat("x", 257)} {
		cfg, err := Load(nil, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Discord.Token = "test-token"
		cfg.MAL.ClientID = client
		if err := cfg.ValidateRuntime(); err == nil || strings.Contains(err.Error(), "DO-NOT-PRINT") {
			t.Fatal("invalid MAL credential was accepted or exposed", err)
		}
	}
}
