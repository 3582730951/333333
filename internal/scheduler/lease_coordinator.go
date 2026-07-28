package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/supervisor"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

var ErrLeaseCoordinatorUnavailable = errors.New("lease coordinator unavailable")

type LeaseResource struct {
	ID    string
	Limit int
}

type LeaseRequest struct {
	AccountID       string
	EstimatedTokens int64
	TokenBudget     int64
	Compaction      bool
	Resources       []LeaseResource
	TTL             time.Duration
}

type CoordinatedLease interface {
	FencingToken() uint64
	Release(context.Context) error
}

// LeaseCoordinator is the single authority for account token and outlet capacity.
// The scheduler's existing cancellation-aware FIFO owns ordering; coordinators make
// the eventual grant atomic across every resource in one request.
type LeaseCoordinator interface {
	TryAcquire(context.Context, LeaseRequest) (CoordinatedLease, leaseBlockReason, error)
	Notifications() <-chan struct{}
	Close() error
}

func NewLeaseCoordinator(cfg config.Config) (LeaseCoordinator, error) {
	if strings.TrimSpace(cfg.RedisURL) == "" {
		return newLocalLeaseCoordinator(), nil
	}
	return newRedisLeaseCoordinator(cfg.RedisURL, cfg.NodeID)
}

type localLeaseCoordinator struct {
	mu        sync.Mutex
	inflight  map[string]int
	tokens    map[string]int64
	resources map[string]int
	fence     atomic.Uint64
	notify    chan struct{}
}

func newLocalLeaseCoordinator() *localLeaseCoordinator {
	return &localLeaseCoordinator{inflight: map[string]int{}, tokens: map[string]int64{}, resources: map[string]int{}, notify: make(chan struct{}, 1)}
}

func (c *localLeaseCoordinator) TryAcquire(_ context.Context, request LeaseRequest) (CoordinatedLease, leaseBlockReason, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, resource := range request.Resources {
		if resource.Limit > 0 && c.resources[resource.ID] >= resource.Limit {
			return nil, leaseBlockConcurrency, nil
		}
	}
	if tokenBudgetLimited(request.TokenBudget, request.Compaction, c.inflight[request.AccountID], c.tokens[request.AccountID], request.EstimatedTokens) {
		return nil, leaseBlockTokenBudget, nil
	}
	c.inflight[request.AccountID]++
	c.tokens[request.AccountID] += request.EstimatedTokens
	for _, resource := range request.Resources {
		c.resources[resource.ID]++
	}
	lease := &localCoordinatedLease{coordinator: c, request: request, fence: c.fence.Add(1)}
	return lease, leaseBlockNone, nil
}

func (c *localLeaseCoordinator) Notifications() <-chan struct{} { return c.notify }
func (c *localLeaseCoordinator) Close() error                   { return nil }

type localCoordinatedLease struct {
	coordinator *localLeaseCoordinator
	request     LeaseRequest
	fence       uint64
	once        sync.Once
}

func (l *localCoordinatedLease) FencingToken() uint64 { return l.fence }

func (l *localCoordinatedLease) Release(context.Context) error {
	l.once.Do(func() {
		c := l.coordinator
		c.mu.Lock()
		if c.inflight[l.request.AccountID] > 0 {
			c.inflight[l.request.AccountID]--
		}
		c.tokens[l.request.AccountID] -= l.request.EstimatedTokens
		if c.tokens[l.request.AccountID] < 0 {
			c.tokens[l.request.AccountID] = 0
		}
		for _, resource := range l.request.Resources {
			if c.resources[resource.ID] > 0 {
				c.resources[resource.ID]--
			}
		}
		c.mu.Unlock()
		select {
		case c.notify <- struct{}{}:
		default:
		}
	})
	return nil
}

const redisLeaseAcquireScript = `
if redis.call('GET', KEYS[5]) ~= ARGV[11 + tonumber(ARGV[7])] then return {-3, 0, 0} end
local expired = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
if #expired > 0 then
  redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
  redis.call('HDEL', KEYS[2], unpack(expired))
end
local count = redis.call('ZCARD', KEYS[1])
local tokens = 0
for _, value in ipairs(redis.call('HVALS', KEYS[2])) do tokens = tokens + tonumber(value) end
local requested = tonumber(ARGV[4])
local budget = tonumber(ARGV[5])
local resources = tonumber(ARGV[7])
for index = 1, resources do
  local key = KEYS[5 + index]
  redis.call('ZREMRANGEBYSCORE', key, '-inf', ARGV[1])
  local limit = tonumber(ARGV[7 + index])
  if limit > 0 and redis.call('ZCARD', key) >= limit then return {-1, count, tokens} end
end
if ARGV[6] == '0' and count > 0 and budget > 0 and tokens + requested > budget then return {-2, count, tokens} end
local fence = redis.call('INCR', KEYS[3])
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[3])
redis.call('HSET', KEYS[2], ARGV[3], ARGV[4])
for index = 1, resources do redis.call('ZADD', KEYS[5 + index], ARGV[2], ARGV[3]) end
redis.call('HSET', KEYS[4], 'fence', fence, 'account', ARGV[8 + resources], 'expires', ARGV[2])
redis.call('PEXPIRE', KEYS[4], ARGV[9 + resources])
redis.call('PUBLISH', ARGV[10 + resources], ARGV[3])
return {fence, count + 1, tokens + requested}
`

const redisLeaseReleaseScript = `
if redis.call('GET', KEYS[4]) ~= ARGV[4] then return -1 end
if redis.call('HGET', KEYS[3], 'fence') ~= ARGV[2] then return 0 end
redis.call('ZREM', KEYS[1], ARGV[1])
redis.call('HDEL', KEYS[2], ARGV[1])
for index = 5, #KEYS do redis.call('ZREM', KEYS[index], ARGV[1]) end
redis.call('DEL', KEYS[3])
redis.call('PUBLISH', ARGV[3], ARGV[1])
return 1
`

const redisLeaseRenewScript = `
if redis.call('GET', KEYS[4]) ~= ARGV[5] then return -1 end
if redis.call('HGET', KEYS[3], 'fence') ~= ARGV[2] then return 0 end
if not redis.call('ZSCORE', KEYS[1], ARGV[1]) then return 0 end
redis.call('ZADD', KEYS[1], ARGV[3], ARGV[1])
for index = 5, #KEYS do redis.call('ZADD', KEYS[index], ARGV[3], ARGV[1]) end
redis.call('HSET', KEYS[3], 'expires', ARGV[3])
redis.call('PEXPIRE', KEYS[3], ARGV[4])
return 1
`

type redisLeaseCoordinator struct {
	client   *redis.Client
	nodeID   string
	prefix   string
	channel  string
	epoch    string
	epochKey string
	notify   chan struct{}
	pubsub   *redis.PubSub
	stop     chan struct{}
	once     sync.Once
}

func newRedisLeaseCoordinator(rawURL, nodeID string) (*redisLeaseCoordinator, error) {
	options, err := redis.ParseURL(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("%w: parse Redis URL: %v", ErrLeaseCoordinatorUnavailable, err)
	}
	client := redis.NewClient(options)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("%w: Redis ping: %v", ErrLeaseCoordinatorUnavailable, err)
	}
	coordinator := &redisLeaseCoordinator{
		client: client, nodeID: strings.TrimSpace(nodeID), prefix: "codex-pool:lease:v1:", channel: "codex-pool:lease:v1:changed",
		notify: make(chan struct{}, 1), stop: make(chan struct{}),
	}
	coordinator.epochKey = coordinator.prefix + "epoch"
	candidateEpoch := uuid.NewString()
	if err = client.SetNX(ctx, coordinator.epochKey, candidateEpoch, 0).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("%w: initialize Redis epoch: %v", ErrLeaseCoordinatorUnavailable, err)
	}
	if coordinator.epoch, err = client.Get(ctx, coordinator.epochKey).Result(); err != nil || strings.TrimSpace(coordinator.epoch) == "" {
		_ = client.Close()
		return nil, fmt.Errorf("%w: read Redis epoch: %v", ErrLeaseCoordinatorUnavailable, err)
	}
	coordinator.pubsub = client.Subscribe(context.Background(), coordinator.channel)
	go coordinator.consumeNotifications()
	return coordinator, nil
}

func leaseKeyPart(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:16])
}

func (c *redisLeaseCoordinator) TryAcquire(ctx context.Context, request LeaseRequest) (CoordinatedLease, leaseBlockReason, error) {
	if request.TTL <= 0 {
		request.TTL = 20 * time.Minute
	}
	now := time.Now()
	expiry := now.Add(request.TTL)
	leaseID := c.nodeID + ":" + uuid.NewString()
	accountPart := leaseKeyPart(request.AccountID)
	keys := []string{
		c.prefix + "account:" + accountPart,
		c.prefix + "tokens:" + accountPart,
		c.prefix + "fence",
		c.prefix + "record:" + leaseKeyPart(leaseID),
		c.epochKey,
	}
	args := []interface{}{now.UnixMilli(), expiry.UnixMilli(), leaseID, request.EstimatedTokens, request.TokenBudget, boolIntString(request.Compaction), len(request.Resources)}
	for _, resource := range request.Resources {
		keys = append(keys, c.prefix+"resource:"+leaseKeyPart(resource.ID))
		args = append(args, resource.Limit)
	}
	args = append(args, request.AccountID, request.TTL.Milliseconds(), c.channel, c.epoch)
	result, err := c.client.Eval(ctx, redisLeaseAcquireScript, keys, args...).Slice()
	if err != nil {
		return nil, leaseBlockCoordinator, fmt.Errorf("%w: Redis acquire: %v", ErrLeaseCoordinatorUnavailable, err)
	}
	if len(result) == 0 {
		return nil, leaseBlockCoordinator, fmt.Errorf("%w: empty Redis acquire result", ErrLeaseCoordinatorUnavailable)
	}
	code, err := redisInt64(result[0])
	if err != nil {
		return nil, leaseBlockCoordinator, fmt.Errorf("%w: invalid Redis acquire result: %v", ErrLeaseCoordinatorUnavailable, err)
	}
	switch code {
	case -1:
		return nil, leaseBlockConcurrency, nil
	case -2:
		return nil, leaseBlockTokenBudget, nil
	case -3:
		return nil, leaseBlockCoordinator, fmt.Errorf("%w: Redis lease epoch changed; restart this node after Redis durability is restored", ErrLeaseCoordinatorUnavailable)
	}
	leaseKeys := []string{keys[0], keys[1], keys[3], keys[4]}
	leaseKeys = append(leaseKeys, keys[5:]...)
	lease := &redisCoordinatedLease{coordinator: c, request: request, id: leaseID, fence: uint64(code), keys: leaseKeys, ttl: request.TTL, stop: make(chan struct{})}
	go lease.renewLoop()
	return lease, leaseBlockNone, nil
}

func boolIntString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func redisInt64(value interface{}) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	case []byte:
		return strconv.ParseInt(string(typed), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected %T", value)
	}
}

func (c *redisLeaseCoordinator) Notifications() <-chan struct{} { return c.notify }

func (c *redisLeaseCoordinator) consumeNotifications() {
	defer supervisor.Recover("redis-lease-notifications")
	channel := c.pubsub.Channel()
	for {
		select {
		case <-c.stop:
			return
		case _, ok := <-channel:
			if !ok {
				return
			}
			select {
			case c.notify <- struct{}{}:
			default:
			}
		}
	}
}

func (c *redisLeaseCoordinator) Close() error {
	var err error
	c.once.Do(func() {
		close(c.stop)
		err = errors.Join(c.pubsub.Close(), c.client.Close())
	})
	return err
}

type redisCoordinatedLease struct {
	coordinator *redisLeaseCoordinator
	request     LeaseRequest
	id          string
	fence       uint64
	keys        []string
	ttl         time.Duration
	stop        chan struct{}
	once        sync.Once
	releaseErr  error
}

func (l *redisCoordinatedLease) FencingToken() uint64 { return l.fence }

func (l *redisCoordinatedLease) renewLoop() {
	defer supervisor.Recover("redis-lease-renewal")
	interval := l.ttl / 3
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-timer.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			expiry := time.Now().Add(l.ttl).UnixMilli()
			result, err := l.coordinator.client.Eval(ctx, redisLeaseRenewScript, l.keys, l.id, strconv.FormatUint(l.fence, 10), expiry, l.ttl.Milliseconds(), l.coordinator.epoch).Int()
			cancel()
			if err != nil || result != 1 {
				select {
				case l.coordinator.notify <- struct{}{}:
				default:
				}
				return
			}
		}
	}
}

func (l *redisCoordinatedLease) Release(ctx context.Context) error {
	l.once.Do(func() {
		close(l.stop)
		var result int
		result, l.releaseErr = l.coordinator.client.Eval(ctx, redisLeaseReleaseScript, l.keys, l.id, strconv.FormatUint(l.fence, 10), l.coordinator.channel, l.coordinator.epoch).Int()
		if l.releaseErr != nil {
			l.releaseErr = fmt.Errorf("%w: Redis release: %v", ErrLeaseCoordinatorUnavailable, l.releaseErr)
		} else if result < 0 {
			l.releaseErr = fmt.Errorf("%w: Redis lease epoch changed before release", ErrLeaseCoordinatorUnavailable)
		}
	})
	return l.releaseErr
}
