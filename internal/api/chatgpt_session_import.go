package api

import (
	"codex-account-pool/internal/accountprovider"
	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/storage"
	"context"
	"errors"
	"fmt"
	"strings"
)

const maxImportedSessionCookieBytes = 64 << 10

func normalizeImportedSessionCookie(explicit string, parsed authparse.ParsedAuth) (string, error) {
	raw := strings.TrimSpace(firstNonEmpty(explicit, parsed.SessionCookie))
	if raw == "" {
		return "", nil
	}
	if len(raw) > maxImportedSessionCookieBytes {
		return "", fmt.Errorf("session cookie exceeds %d bytes", maxImportedSessionCookieBytes)
	}
	if strings.ContainsAny(raw, "\r\n") {
		return "", errors.New("session cookie must be a single Cookie header value")
	}
	if len(raw) >= len("cookie:") && strings.EqualFold(raw[:len("cookie:")], "cookie:") {
		raw = strings.TrimSpace(raw[len("cookie:"):])
	}
	if raw == "" {
		return "", errors.New("session cookie is empty")
	}
	if !strings.Contains(raw, "=") {
		raw = "__Secure-next-auth.session-token=" + raw
	}
	return raw, nil
}

func importedAuthWarnings(parsed authparse.ParsedAuth, sessionCookie string) []string {
	if parsed.CredentialMode != authparse.CredentialModeChatGPTAuthTokens {
		return nil
	}
	warnings := make([]string, 0, 2)
	if parsed.SyntheticIDToken {
		warnings = append(warnings, "id_token is a local metadata-only compatibility JWT; upstream authentication always uses access_token")
	}
	if strings.TrimSpace(parsed.RefreshToken) == "" {
		if sessionCookie == "" {
			warnings = append(warnings, "Web session has no refresh_token or session cookie; re-import a fresh session before access_token expiry")
		} else {
			warnings = append(warnings, "Web session has no refresh_token; access_token renewal will use the encrypted session cookie")
		}
	}
	return warnings
}

func (s *Server) storeImportedSessionCookie(ctx context.Context, accountID, sessionCookie string) error {
	if sessionCookie == "" {
		return nil
	}
	return s.store.SetSessionCookie(ctx, accountID, sessionCookie)
}

func (s *Server) updateExistingExternalChatGPTTokens(ctx context.Context, existing storage.Account, parsed authparse.ParsedAuth, sessionCookie string) (storage.Account, bool, error) {
	if parsed.CredentialMode != authparse.CredentialModeChatGPTAuthTokens {
		return existing, false, nil
	}
	current, err := s.store.GetToken(ctx, existing.ID)
	if err != nil {
		return storage.Account{}, false, err
	}
	// Never replace a refreshable OAuth credential with a less durable pasted Web session, regardless of legacy credential_mode metadata.
	if strings.TrimSpace(current.RefreshToken) != "" {
		return existing, false, nil
	}
	replacement := accountTokenFromParsed(parsed, "")
	replacement.AccountID = existing.ID
	if current.ExpiresAt > 0 && replacement.ExpiresAt > 0 && replacement.ExpiresAt < current.ExpiresAt {
		return storage.Account{}, false, errors.New("refusing to replace a newer Web session access token with an older one")
	}
	if parsed.UpstreamAccountID != "" {
		existing.UpstreamAccountID = parsed.UpstreamAccountID
	}
	if parsed.ChatGPTUserID != "" {
		existing.ChatGPTUserID = parsed.ChatGPTUserID
	}
	if parsed.Email != "" {
		existing.Email = parsed.Email
	}
	if parsed.PlanType != "" {
		existing.PlanType = parsed.PlanType
	}
	existing.IsFedramp = parsed.IsFedramp
	if existing.Provider == "" || existing.Provider == accountprovider.UnknownProvider {
		existing.Provider = "codex"
	}
	if err := s.store.UpsertAccount(ctx, existing, replacement); err != nil {
		return storage.Account{}, false, err
	}
	if err := s.storeImportedSessionCookie(ctx, existing.ID, sessionCookie); err != nil {
		return storage.Account{}, false, err
	}
	return existing, true, nil
}
