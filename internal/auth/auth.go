package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"

	"harborlight/internal/config"
	"harborlight/internal/store"
)

// Service 认证服务：OAuth 登录 + 签名 Cookie 会话。
// 配置了 Redis 时会话存 Redis（带 TTL），否则存内存。
type Service struct {
	cfg      *config.Config
	oauth    *oauth2.Config
	sessions *sessionStore
}

func New(cfg *config.Config, rdb *redis.Client) *Service {
	s := &Service{cfg: cfg, sessions: newSessionStore(rdb)}
	if cfg.OAuthEnabled() {
		var oc *oauth2.Config
		switch cfg.Provider {
		case "github":
			oc = &oauth2.Config{
				ClientID:     cfg.ClientID,
				ClientSecret: cfg.ClientSecret,
				RedirectURL:  cfg.OAuthRedirectURL(),
				Scopes:       strings.Fields(orDefault(cfg.Scopes, "read:user user:email")),
				Endpoint:     github.Endpoint,
			}
		case "generic":
			oc = &oauth2.Config{
				ClientID:     cfg.ClientID,
				ClientSecret: cfg.ClientSecret,
				RedirectURL:  cfg.OAuthRedirectURL(),
				Scopes:       strings.Fields(orDefault(cfg.Scopes, "openid profile email")),
				Endpoint: oauth2.Endpoint{
					AuthURL:  cfg.AuthorizeURL,
					TokenURL: cfg.TokenURL,
				},
			}
		}
		s.oauth = oc
	}
	return s
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// ---- OAuth ----

func (s *Service) AuthCodeURL(state string) string { return s.oauth.AuthCodeURL(state) }

func (s *Service) OAuthEnabled() bool { return s.oauth != nil }

// Exchange 用 code 换 token 并拉取用户信息。
func (s *Service) Exchange(ctx context.Context, code string) (*store.User, error) {
	if s.oauth == nil {
		return nil, errors.New("OAuth 未配置")
	}
	tok, err := s.oauth.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("换取 token 失败: %w", err)
	}
	if s.cfg.Provider == "github" {
		return s.fetchGitHubUser(ctx, tok)
	}
	return s.fetchGenericUser(ctx, tok)
}

func (s *Service) fetchGitHubUser(ctx context.Context, tok *oauth2.Token) (*store.User, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var u struct {
		Login     string `json:"login"`
		Email     string `json:"email"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, err
	}
	email := u.Email
	if email == "" {
		email = u.Login + "@users.noreply.github.com"
	}
	return &store.User{Email: email, Name: orDefault(u.Name, u.Login), Avatar: u.AvatarURL}, nil
}

func (s *Service) fetchGenericUser(ctx context.Context, tok *oauth2.Token) (*store.User, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", s.cfg.UserInfoURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var u map[string]any
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, err
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := u[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	email := str("email", "preferred_username", "sub")
	if email == "" {
		return nil, errors.New("OIDC 响应中缺少 email/sub 字段")
	}
	return &store.User{
		Email:  email,
		Name:   orDefault(str("name", "preferred_username"), email),
		Avatar: str("picture"),
	}, nil
}

// ---- 会话 ----

type Session struct {
	UserID    string
	Email     string
	Name      string
	Avatar    string
	Role      string
	ExpiresAt time.Time
}

const (
	cookieName    = "hl_session"
	sessionTTL    = 24 * time.Hour
	stateTTL      = 10 * time.Minute
)

func (s *Service) CreateSession(w http.ResponseWriter, r *http.Request, u *store.User) error {
	sess := &Session{
		UserID:    u.ID,
		Email:     u.Email,
		Name:      u.Name,
		Avatar:    u.Avatar,
		Role:      u.Role,
		ExpiresAt: time.Now().Add(sessionTTL),
	}
	id := uuid.NewString()
	if err := s.sessions.put(r.Context(), id, sess); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return nil
}

func (s *Service) GetSession(r *http.Request) *Session {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	sess, err := s.sessions.get(r.Context(), c.Value)
	if err != nil || sess == nil {
		return nil
	}
	if time.Now().After(sess.ExpiresAt) {
		s.sessions.del(r.Context(), c.Value)
		return nil
	}
	return sess
}

func (s *Service) DestroySession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.sessions.del(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
}

// OAuthState 保存/校验防 CSRF state（同样利用 session store）。
func (s *Service) SaveState(r *http.Request) (string, error) {
	state := uuid.NewString()
	return state, s.sessions.putState(r.Context(), state)
}

func (s *Service) CheckState(r *http.Request, state string) bool {
	if state == "" {
		return false
	}
	return s.sessions.popState(r.Context(), state)
}
