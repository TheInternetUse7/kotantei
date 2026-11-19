// Package config loads non-web bot configuration and environment overrides.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Duration time.Duration

func (d Duration) Value() time.Duration { return time.Duration(d) }

type Discord struct {
	Token string `json:"token"`
}
type MAL struct {
	Enabled         bool     `json:"enabled"`
	TenraiEnabled   bool     `json:"tenrai_enabled"`
	ClientID        string   `json:"client_id"`
	CycleDelay      Duration `json:"cycle_delay"`
	RequestInterval Duration `json:"request_interval"`
}
type AniList struct {
	Enabled           bool     `json:"enabled"`
	CycleDelay        Duration `json:"cycle_delay"`
	RequestsPerMinute int      `json:"requests_per_minute"`
}
type Storage struct {
	Database       string `json:"database"`
	TanteiDatabase string `json:"tantei_database"`
}
type Project struct {
	SourceURL string `json:"source_url"`
}
type Config struct {
	Project Project `json:"project"`
	Discord Discord `json:"discord"`
	MAL     MAL     `json:"mal"`
	AniList AniList `json:"anilist"`
	Storage Storage `json:"storage"`
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return errors.New("duration must be a string such as 50s or 2m")
	}
	value, err := time.ParseDuration(text)
	if err != nil {
		return errors.New("invalid duration; use a value such as 50s or 2m")
	}
	*d = Duration(value)
	return nil
}

// Load accepts JSONC (or plain JSON), or a nil reader for defaults. Environment
// overrides allow an explicitly empty value, e.g. to disable legacy auto-import.
func Load(reader io.Reader, lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		MAL:     MAL{Enabled: true, CycleDelay: Duration(50 * time.Second), RequestInterval: Duration(3 * time.Second)},
		AniList: AniList{Enabled: true, CycleDelay: Duration(90 * time.Second), RequestsPerMinute: 29},
		Storage: Storage{Database: "data/kotantei.db", TanteiDatabase: "data/data.db"},
	}
	if reader != nil {
		const limit = 1024 * 1024
		data, err := io.ReadAll(io.LimitReader(reader, limit+1))
		if err != nil {
			return Config{}, errors.New("cannot read configuration")
		}
		if len(data) > limit {
			return Config{}, errors.New("configuration exceeds 1 MiB")
		}
		data, err = normalizeJSONC(data)
		if err != nil {
			return Config{}, err
		}
		var tree any
		if err := json.Unmarshal(data, &tree); err != nil {
			return Config{}, errors.New("invalid configuration JSONC")
		}
		if _, ok := tree.(map[string]any); !ok || containsNull(tree) {
			return Config{}, errors.New("configuration must be an object without null values")
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err != nil {
			return Config{}, fmt.Errorf("invalid configuration: %w", err)
		}
	}
	if lookup != nil {
		for key, destination := range map[string]*string{
			"KOTANTEI_SOURCE_URL":      &cfg.Project.SourceURL,
			"KOTANTEI_DISCORD_TOKEN":   &cfg.Discord.Token,
			"KOTANTEI_MAL_CLIENT_ID":   &cfg.MAL.ClientID,
			"KOTANTEI_DATABASE":        &cfg.Storage.Database,
			"KOTANTEI_TANTEI_DATABASE": &cfg.Storage.TanteiDatabase,
		} {
			if value, ok := lookup(key); ok {
				*destination = strings.TrimSpace(value)
			}
		}
		// The generic name takes precedence, including an explicitly empty value.
		if value, ok := lookup("KOTANTEI_LEGACY_DATABASE"); ok {
			cfg.Storage.TanteiDatabase = strings.TrimSpace(value)
		}
		for key, destination := range map[string]*bool{
			"KOTANTEI_ANILIST_ENABLED":    &cfg.AniList.Enabled,
			"KOTANTEI_MAL_ENABLED":        &cfg.MAL.Enabled,
			"KOTANTEI_MAL_TENRAI_ENABLED": &cfg.MAL.TenraiEnabled,
		} {
			if value, ok := lookup(key); ok {
				switch strings.ToLower(strings.TrimSpace(value)) {
				case "true":
					*destination = true
				case "false":
					*destination = false
				default:
					return Config{}, fmt.Errorf("%s must be true or false", key)
				}
			}
		}
		for key, destination := range map[string]*Duration{
			"KOTANTEI_ANILIST_CYCLE_DELAY":  &cfg.AniList.CycleDelay,
			"KOTANTEI_MAL_CYCLE_DELAY":      &cfg.MAL.CycleDelay,
			"KOTANTEI_MAL_REQUEST_INTERVAL": &cfg.MAL.RequestInterval,
		} {
			if value, ok := lookup(key); ok {
				duration, err := time.ParseDuration(strings.TrimSpace(value))
				if err != nil || duration <= 0 {
					return Config{}, fmt.Errorf("%s must be a positive duration such as 90s or 2m", key)
				}
				*destination = Duration(duration)
			}
		}
		if value, ok := lookup("KOTANTEI_ANILIST_REQUESTS_PER_MINUTE"); ok {
			limit, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || limit < 1 || limit > 29 {
				return Config{}, errors.New("KOTANTEI_ANILIST_REQUESTS_PER_MINUTE must be an integer between 1 and 29")
			}
			cfg.AniList.RequestsPerMinute = limit
		}
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func containsNull(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case map[string]any:
		for _, child := range value {
			if containsNull(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsNull(child) {
				return true
			}
		}
	}
	return false
}

func (cfg Config) validate() error {
	if cfg.Project.SourceURL != "" {
		u, err := url.Parse(cfg.Project.SourceURL)
		if err != nil || len(cfg.Project.SourceURL) > 2048 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			return errors.New("project.source_url must be an HTTPS source-download URL without credentials, at most 2048 bytes")
		}
	}
	if strings.TrimSpace(cfg.Storage.Database) == "" {
		return errors.New("storage.database must not be empty")
	}
	if cfg.Storage.TanteiDatabase != "" {
		target, err := filepath.Abs(cfg.Storage.Database)
		if err != nil {
			return errors.New("invalid storage.database path")
		}
		source, err := filepath.Abs(cfg.Storage.TanteiDatabase)
		if err != nil {
			return errors.New("invalid storage.tantei_database path")
		}
		if target == source {
			return errors.New("destination and legacy database paths must differ")
		}
	}
	if cfg.MAL.CycleDelay <= 0 || cfg.MAL.RequestInterval <= 0 {
		return errors.New("MAL delays must be positive")
	}
	if cfg.AniList.CycleDelay <= 0 {
		return errors.New("AniList cycle delay must be positive")
	}
	if cfg.AniList.RequestsPerMinute < 1 || cfg.AniList.RequestsPerMinute > 29 {
		return errors.New("AniList requests_per_minute must be between 1 and 29")
	}
	return nil
}

// ValidateRuntime is separate so import/inspection never require credentials.
func (cfg Config) ValidateRuntime() error {
	if err := cfg.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Discord.Token) == "" {
		return errors.New("set KOTANTEI_DISCORD_TOKEN or discord.token before running the bot")
	}
	token := strings.TrimPrefix(strings.TrimSpace(cfg.Discord.Token), "Bot ")
	if token == "" || len(token) > 512 || strings.IndexFunc(token, func(r rune) bool { return r <= 32 || r > 126 }) >= 0 {
		return errors.New("discord.token must be a valid bot token without header whitespace")
	}
	if !cfg.MAL.Enabled && !cfg.AniList.Enabled {
		return errors.New("enable at least one provider before running the bot")
	}
	if cfg.MAL.Enabled && strings.TrimSpace(cfg.MAL.ClientID) == "" {
		return errors.New("set KOTANTEI_MAL_CLIENT_ID or mal.client_id when MAL is enabled")
	}
	if cfg.MAL.Enabled && (cfg.MAL.ClientID != strings.TrimSpace(cfg.MAL.ClientID) || strings.ContainsAny(cfg.MAL.ClientID, "\r\n") || len(cfg.MAL.ClientID) > 256) {
		return errors.New("mal.client_id must be a valid client ID without header whitespace")
	}
	return nil
}
