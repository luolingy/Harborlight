package auth

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// sessionStore 会话存储：Redis 可用时持久化到 Redis（支持多实例部署），
// 否则退化为进程内存 map。
type sessionStore struct {
	rdb  *redis.Client
	mu   sync.RWMutex
	mem  map[string]jsonish
	ttl  time.Duration
}

// jsonish 统一内存/Redis 两种存储的序列化形式。
type jsonish struct{ v []byte }

var errNoStore = errors.New("session store closed")

func newSessionStore(rdb *redis.Client) *sessionStore {
	return &sessionStore{rdb: rdb, mem: map[string]jsonish{}, ttl: sessionTTL}
}

const (
	keySess  = "hl:sess:"
	keyState = "hl:state:"
)

func (s *sessionStore) put(ctx context.Context, id string, sess *Session) error {
	b, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	return s.set(ctx, keySess+id, b)
}

func (s *sessionStore) get(ctx context.Context, id string) (*Session, error) {
	b, err := s.getBytes(ctx, keySess+id)
	if err != nil || b == nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(b, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *sessionStore) del(ctx context.Context, id string) {
	s.mu.Lock()
	delete(s.mem, keySess+id)
	s.mu.Unlock()
	if s.rdb != nil {
		s.rdb.Del(ctx, keySess+id)
	}
}

func (s *sessionStore) putState(ctx context.Context, state string) error {
	return s.set(ctx, keyState+state, []byte("1"))
}

func (s *sessionStore) popState(ctx context.Context, state string) bool {
	ok := false
	if s.rdb != nil {
		n, err := s.rdb.Del(ctx, keyState+state).Result()
		ok = err == nil && n == 1
	} else {
		s.mu.Lock()
		_, ok = s.mem[keyState+state]
		delete(s.mem, keyState+state)
		s.mu.Unlock()
	}
	return ok
}

func (s *sessionStore) set(ctx context.Context, key string, b []byte) error {
	if s.rdb != nil {
		return s.rdb.Set(ctx, key, b, s.ttl).Err()
	}
	s.mu.Lock()
	s.mem[key] = jsonish{v: b}
	s.mu.Unlock()
	time.AfterFunc(s.ttl, func() { // 内存版 TTL 清理
		s.mu.Lock()
		delete(s.mem, key)
		s.mu.Unlock()
	})
	return nil
}

func (s *sessionStore) getBytes(ctx context.Context, key string) ([]byte, error) {
	if s.rdb != nil {
		v, err := s.rdb.Get(ctx, key).Bytes()
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return v, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.mem[key]
	if !ok {
		return nil, nil
	}
	return j.v, nil
}
