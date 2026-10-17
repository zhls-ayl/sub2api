package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"golang.org/x/sync/singleflight"
)

const (
	kiroMixedToolModeEnabled  = "enabled"
	kiroMixedToolModeDisabled = "disabled"
	kiroMixedToolModeDefault  = "default"
)

// KiroMixedToolWebSearchConfig is the global switch for Kiro mixed-tool
// web_search. GroupModes overrides it per group: "enabled" or "disabled".
// A missing group follows Enabled. The zero value is off.
type KiroMixedToolWebSearchConfig struct {
	Enabled    bool              `json:"enabled"`
	GroupModes map[string]string `json:"group_modes,omitempty"`
}

const sfKeyKiroMixedToolWebSearch = "kiro_mixed_tool_web_search_config"

type cachedKiroMixedToolWebSearchConfig struct {
	config    *KiroMixedToolWebSearchConfig
	expiresAt int64
}

var kiroMixedToolWebSearchCache atomic.Value // *cachedKiroMixedToolWebSearchConfig
var kiroMixedToolWebSearchSF singleflight.Group

const (
	kiroMixedToolWebSearchCacheTTL  = 60 * time.Second
	kiroMixedToolWebSearchDBTimeout = 5 * time.Second
)

func (s *SettingService) GetKiroMixedToolWebSearchConfig(ctx context.Context) (*KiroMixedToolWebSearchConfig, error) {
	if cached := kiroMixedToolWebSearchCache.Load(); cached != nil {
		if c, ok := cached.(*cachedKiroMixedToolWebSearchConfig); ok && time.Now().UnixNano() < c.expiresAt {
			return cloneKiroMixedToolWebSearchConfig(c.config), nil
		}
	}
	result, err, _ := kiroMixedToolWebSearchSF.Do(sfKeyKiroMixedToolWebSearch, func() (any, error) {
		return s.loadKiroMixedToolWebSearchConfig()
	})
	if err != nil {
		return &KiroMixedToolWebSearchConfig{}, err
	}
	if cfg, ok := result.(*KiroMixedToolWebSearchConfig); ok {
		return cloneKiroMixedToolWebSearchConfig(cfg), nil
	}
	return &KiroMixedToolWebSearchConfig{}, nil
}

func (s *SettingService) loadKiroMixedToolWebSearchConfig() (*KiroMixedToolWebSearchConfig, error) {
	dbCtx, cancel := context.WithTimeout(context.Background(), kiroMixedToolWebSearchDBTimeout)
	defer cancel()
	raw, err := s.settingRepo.GetValue(dbCtx, SettingKeyKiroMixedToolWebSearchConfig)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			cfg := &KiroMixedToolWebSearchConfig{}
			storeKiroMixedToolWebSearchConfig(cfg, kiroMixedToolWebSearchCacheTTL)
			return cfg, nil
		}
		return &KiroMixedToolWebSearchConfig{}, err
	}
	cfg := parseKiroMixedToolWebSearchConfig(raw)
	storeKiroMixedToolWebSearchConfig(cfg, kiroMixedToolWebSearchCacheTTL)
	return cfg, nil
}

func storeKiroMixedToolWebSearchConfig(cfg *KiroMixedToolWebSearchConfig, ttl time.Duration) {
	kiroMixedToolWebSearchCache.Store(&cachedKiroMixedToolWebSearchConfig{
		config:    cloneKiroMixedToolWebSearchConfig(cfg),
		expiresAt: time.Now().Add(ttl).UnixNano(),
	})
}

func parseKiroMixedToolWebSearchConfig(raw string) *KiroMixedToolWebSearchConfig {
	cfg := &KiroMixedToolWebSearchConfig{}
	if strings.TrimSpace(raw) == "" {
		return cfg
	}
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		slog.Warn("kiro mixed web search: failed to parse config JSON", "error", err)
		return &KiroMixedToolWebSearchConfig{}
	}
	cfg.GroupModes = normalizeStoredGroupModes(cfg.GroupModes)
	return cfg
}

func normalizeStoredGroupModes(modes map[string]string) map[string]string {
	if len(modes) == 0 {
		return nil
	}
	out := make(map[string]string, len(modes))
	for id, mode := range modes {
		id = strings.TrimSpace(id)
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(mode)) {
		case kiroMixedToolModeEnabled:
			out[id] = kiroMixedToolModeEnabled
		case kiroMixedToolModeDisabled:
			out[id] = kiroMixedToolModeDisabled
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneKiroMixedToolWebSearchConfig(cfg *KiroMixedToolWebSearchConfig) *KiroMixedToolWebSearchConfig {
	if cfg == nil {
		return &KiroMixedToolWebSearchConfig{}
	}
	cloned := &KiroMixedToolWebSearchConfig{Enabled: cfg.Enabled}
	if len(cfg.GroupModes) > 0 {
		cloned.GroupModes = make(map[string]string, len(cfg.GroupModes))
		for k, v := range cfg.GroupModes {
			cloned.GroupModes[k] = v
		}
	}
	return cloned
}

// SaveKiroMixedToolWebSearchConfig persists the global switch.
// groupModes == nil keeps the modes already stored. An empty map clears them.
func (s *SettingService) SaveKiroMixedToolWebSearchConfig(ctx context.Context, enabled bool, groupModes *map[string]string) (*KiroMixedToolWebSearchConfig, error) {
	current, err := s.GetKiroMixedToolWebSearchConfig(ctx)
	if err != nil {
		return nil, err
	}
	next := &KiroMixedToolWebSearchConfig{Enabled: enabled, GroupModes: current.GroupModes}
	if groupModes != nil {
		if err := validateKiroMixedToolGroupModes(*groupModes); err != nil {
			return nil, infraerrors.BadRequest("INVALID_KIRO_MIXED_TOOL_WEB_SEARCH", err.Error())
		}
		next.GroupModes = normalizeStoredGroupModes(*groupModes)
	}
	if err := s.persistKiroMixedToolWebSearchConfig(ctx, next); err != nil {
		return nil, err
	}
	return cloneKiroMixedToolWebSearchConfig(next), nil
}

func (s *SettingService) SetKiroMixedToolWebSearchGroupMode(ctx context.Context, groupID int64, mode string) (*KiroMixedToolWebSearchConfig, error) {
	if groupID <= 0 {
		return nil, infraerrors.BadRequest("INVALID_GROUP_ID", "group id is required")
	}
	normalized, ok := normalizeKiroMixedToolGroupMode(mode)
	if !ok {
		return nil, infraerrors.BadRequest("INVALID_KIRO_MIXED_TOOL_WEB_SEARCH", "mode must be default, enabled, or disabled")
	}
	current, err := s.GetKiroMixedToolWebSearchConfig(ctx)
	if err != nil {
		return nil, err
	}
	if current.GroupModes == nil {
		current.GroupModes = map[string]string{}
	}
	key := strconv.FormatInt(groupID, 10)
	if normalized == kiroMixedToolModeDefault {
		delete(current.GroupModes, key)
	} else {
		current.GroupModes[key] = normalized
	}
	current.GroupModes = normalizeStoredGroupModes(current.GroupModes)
	if err := s.persistKiroMixedToolWebSearchConfig(ctx, current); err != nil {
		return nil, err
	}
	return cloneKiroMixedToolWebSearchConfig(current), nil
}

func (s *SettingService) persistKiroMixedToolWebSearchConfig(ctx context.Context, cfg *KiroMixedToolWebSearchConfig) error {
	if cfg == nil {
		cfg = &KiroMixedToolWebSearchConfig{}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("kiro mixed web search: marshal config: %w", err)
	}
	if err := s.settingRepo.Set(ctx, SettingKeyKiroMixedToolWebSearchConfig, string(data)); err != nil {
		return fmt.Errorf("kiro mixed web search: save config: %w", err)
	}
	kiroMixedToolWebSearchSF.Forget(sfKeyKiroMixedToolWebSearch)
	storeKiroMixedToolWebSearchConfig(cfg, kiroMixedToolWebSearchCacheTTL)
	return nil
}

func validateKiroMixedToolGroupModes(modes map[string]string) error {
	for id, mode := range modes {
		id = strings.TrimSpace(id)
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			return fmt.Errorf("invalid group id %q", id)
		}
		if mode != kiroMixedToolModeEnabled && mode != kiroMixedToolModeDisabled {
			return fmt.Errorf("invalid mode %q for group %s", mode, id)
		}
	}
	return nil
}

func normalizeKiroMixedToolGroupMode(mode string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", kiroMixedToolModeDefault:
		return kiroMixedToolModeDefault, true
	case kiroMixedToolModeEnabled:
		return kiroMixedToolModeEnabled, true
	case kiroMixedToolModeDisabled:
		return kiroMixedToolModeDisabled, true
	default:
		return "", false
	}
}

// kiroMixedToolWebSearchActive resolves the effective switch.
// Non-kiro groups are always off. A group mode of enabled/disabled overrides
// the global flag. Missing config and a missing group mode follow Enabled,
// which defaults to false.
func kiroMixedToolWebSearchActive(cfg *KiroMixedToolWebSearchConfig, group *Group) bool {
	if group != nil {
		platform := strings.TrimSpace(group.Platform)
		if platform != "" && platform != PlatformKiro {
			return false
		}
		if cfg != nil && group.ID > 0 && len(cfg.GroupModes) > 0 {
			switch cfg.GroupModes[strconv.FormatInt(group.ID, 10)] {
			case kiroMixedToolModeEnabled:
				return true
			case kiroMixedToolModeDisabled:
				return false
			}
		}
	}
	if cfg == nil {
		return false
	}
	return cfg.Enabled
}

func (s *GatewayService) kiroMixedToolWebSearchEnabled(ctx context.Context, group *Group) bool {
	if s == nil || s.settingService == nil {
		return false
	}
	cfg, err := s.settingService.GetKiroMixedToolWebSearchConfig(ctx)
	if err != nil || cfg == nil {
		return false
	}
	return kiroMixedToolWebSearchActive(cfg, group)
}
