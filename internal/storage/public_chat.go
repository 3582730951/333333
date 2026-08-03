package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

const (
	PublicChatRouteUserGroup        = "user_group"
	PublicChatRouteAccountPoolGroup = "account_pool_group"
	DefaultPublicChatModel          = "gpt-5.6-sol"
	DefaultPublicChatHistory        = 24
	DefaultPublicChatRateLimit      = 30
	MaxPublicChatHistory            = 100
	MaxPublicChatRateLimit          = 600
)

var publicChatSlugRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{1,63}$`)

// PublicChatLink is a no-login, browser-facing chat entrypoint. It never stores
// an API key; the server resolves the configured route internally and applies
// the selected user/account-pool group policy before forwarding inference.
type PublicChatLink struct {
	ID                 string `json:"id"`
	Slug               string `json:"slug"`
	Name               string `json:"name"`
	Enabled            bool   `json:"enabled"`
	RouteType          string `json:"route_type"`
	UserGroupID        string `json:"user_group_id,omitempty"`
	GroupName          string `json:"group_name,omitempty"`
	Model              string `json:"model"`
	Title              string `json:"title"`
	WelcomeMessage     string `json:"welcome_message"`
	MaxHistoryMessages int    `json:"max_history_messages"`
	RateLimitPerMinute int    `json:"rate_limit_per_minute"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
}

func NormalizePublicChatSlug(value string) (string, error) {
	slug := strings.ToLower(strings.TrimSpace(value))
	if slug == "" {
		return "", errors.New("public chat slug required")
	}
	if !publicChatSlugRE.MatchString(slug) {
		return "", errors.New("public chat slug must be 2-64 characters and contain only letters, numbers, underscore, or dash")
	}
	return slug, nil
}

func (s *Store) normalizePublicChatLink(ctx context.Context, link PublicChatLink) (PublicChatLink, error) {
	link.ID = strings.TrimSpace(link.ID)
	if link.ID == "" {
		link.ID = "chat_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	slug, err := NormalizePublicChatSlug(link.Slug)
	if err != nil {
		return PublicChatLink{}, err
	}
	link.Slug = slug
	link.Name = strings.TrimSpace(link.Name)
	if link.Name == "" {
		link.Name = link.Slug
	}
	link.Title = strings.TrimSpace(link.Title)
	if link.Title == "" {
		link.Title = link.Name
	}
	link.WelcomeMessage = strings.TrimSpace(link.WelcomeMessage)
	link.Model = strings.TrimSpace(link.Model)
	if link.Model == "" {
		link.Model = DefaultPublicChatModel
	}
	if len(link.Model) > 200 {
		return PublicChatLink{}, errors.New("public chat model is too long")
	}
	link.RouteType = strings.ToLower(strings.TrimSpace(link.RouteType))
	if link.RouteType == "" {
		if strings.TrimSpace(link.UserGroupID) != "" {
			link.RouteType = PublicChatRouteUserGroup
		} else {
			link.RouteType = PublicChatRouteAccountPoolGroup
		}
	}
	link.UserGroupID = strings.TrimSpace(link.UserGroupID)
	link.GroupName = strings.TrimSpace(link.GroupName)
	switch link.RouteType {
	case PublicChatRouteUserGroup:
		if link.UserGroupID == "" {
			return PublicChatLink{}, errors.New("public chat user_group_id required")
		}
		var exists int
		if err := s.rdb.QueryRowContext(ctx, `SELECT 1 FROM user_groups WHERE id = ?`, link.UserGroupID).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return PublicChatLink{}, fmt.Errorf("user group %q not found", link.UserGroupID)
			}
			return PublicChatLink{}, err
		}
		link.GroupName = ""
	case PublicChatRouteAccountPoolGroup:
		if link.GroupName == "" {
			return PublicChatLink{}, errors.New("public chat group_name required")
		}
		var exists int
		if err := s.rdb.QueryRowContext(ctx, `SELECT 1 FROM groups WHERE name = ?`, link.GroupName).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return PublicChatLink{}, fmt.Errorf("account pool group %q not found", link.GroupName)
			}
			return PublicChatLink{}, err
		}
		link.UserGroupID = ""
	default:
		return PublicChatLink{}, fmt.Errorf("unsupported public chat route_type %q", link.RouteType)
	}
	if link.MaxHistoryMessages <= 0 {
		link.MaxHistoryMessages = DefaultPublicChatHistory
	}
	if link.MaxHistoryMessages > MaxPublicChatHistory {
		link.MaxHistoryMessages = MaxPublicChatHistory
	}
	if link.RateLimitPerMinute <= 0 {
		link.RateLimitPerMinute = DefaultPublicChatRateLimit
	}
	if link.RateLimitPerMinute > MaxPublicChatRateLimit {
		link.RateLimitPerMinute = MaxPublicChatRateLimit
	}
	return link, nil
}

const publicChatCols = `id, slug, name, enabled, route_type, user_group_id, group_name, model, title, welcome_message, max_history_messages, rate_limit_per_minute, created_at, updated_at`

func scanPublicChatLink(scan func(...interface{}) error) (PublicChatLink, error) {
	var link PublicChatLink
	var enabled int
	err := scan(
		&link.ID, &link.Slug, &link.Name, &enabled, &link.RouteType, &link.UserGroupID, &link.GroupName,
		&link.Model, &link.Title, &link.WelcomeMessage, &link.MaxHistoryMessages, &link.RateLimitPerMinute,
		&link.CreatedAt, &link.UpdatedAt,
	)
	link.Enabled = enabled != 0
	return link, err
}

func (s *Store) ListPublicChatLinks(ctx context.Context) ([]PublicChatLink, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+publicChatCols+` FROM public_chat_links ORDER BY updated_at DESC, slug ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PublicChatLink{}
	for rows.Next() {
		link, scanErr := scanPublicChatLink(rows.Scan)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, link)
	}
	return out, rows.Err()
}

func (s *Store) GetPublicChatLink(ctx context.Context, id string) (PublicChatLink, bool, error) {
	link, err := scanPublicChatLink(s.rdb.QueryRowContext(ctx, `SELECT `+publicChatCols+` FROM public_chat_links WHERE id = ?`, strings.TrimSpace(id)).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return PublicChatLink{}, false, nil
	}
	return link, err == nil, err
}

func (s *Store) GetPublicChatLinkBySlug(ctx context.Context, slug string) (PublicChatLink, bool, error) {
	normalized, err := NormalizePublicChatSlug(slug)
	if err != nil {
		return PublicChatLink{}, false, nil
	}
	link, err := scanPublicChatLink(s.rdb.QueryRowContext(ctx, `SELECT `+publicChatCols+` FROM public_chat_links WHERE slug = ?`, normalized).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return PublicChatLink{}, false, nil
	}
	return link, err == nil, err
}

func (s *Store) UpsertPublicChatLink(ctx context.Context, link PublicChatLink) (PublicChatLink, error) {
	link, err := s.normalizePublicChatLink(ctx, link)
	if err != nil {
		return PublicChatLink{}, err
	}
	now := Now()
	if link.CreatedAt == 0 {
		link.CreatedAt = now
	}
	link.UpdatedAt = now
	_, err = s.db.ExecContext(ctx, `
INSERT INTO public_chat_links(id, slug, name, enabled, route_type, user_group_id, group_name, model, title, welcome_message, max_history_messages, rate_limit_per_minute, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
 slug=excluded.slug,
 name=excluded.name,
 enabled=excluded.enabled,
 route_type=excluded.route_type,
 user_group_id=excluded.user_group_id,
 group_name=excluded.group_name,
 model=excluded.model,
 title=excluded.title,
 welcome_message=excluded.welcome_message,
 max_history_messages=excluded.max_history_messages,
 rate_limit_per_minute=excluded.rate_limit_per_minute,
 updated_at=excluded.updated_at`,
		link.ID, link.Slug, link.Name, boolInt(link.Enabled), link.RouteType, link.UserGroupID, link.GroupName,
		link.Model, link.Title, link.WelcomeMessage, link.MaxHistoryMessages, link.RateLimitPerMinute,
		link.CreatedAt, link.UpdatedAt)
	if err != nil {
		return PublicChatLink{}, err
	}
	return link, nil
}

func (s *Store) DeletePublicChatLink(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM public_chat_links WHERE id = ?`, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}
