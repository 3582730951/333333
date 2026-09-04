package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
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

// LeaseCandidateRequest is one physical-account candidate in a same-tier
// multi-target acquisition. BalancePriority is an optional soft overlay: +1 is
// relief, -1 is above-threshold, and 0 preserves the historical ordering. It is
// considered before target pressure/account load only when the scheduler has a
// strict balancing decision; candidates are never filtered here.
type LeaseCandidateRequest struct {
	ChoiceKey       string
	Request         LeaseRequest
	AccountScore    float64
	Pending         int
	BalancePriority int
}

type CoordinatedLeaseSelection struct {
	Lease     CoordinatedLease
	ChoiceKey string
	AccountID string
}

// MultiLeaseCoordinator is implemented by coordinators that select and lease
// across all same-tier targets in one critical section / Lua invocation.
type MultiLeaseCoordinator interface {
	TryAcquireAcross(context.Context, []LeaseCandidateRequest) (CoordinatedLeaseSelection, leaseBlockReason, error)
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
	rr        uint64
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

func (c *localLeaseCoordinator) TryAcquireAcross(_ context.Context, candidates []LeaseCandidateRequest) (CoordinatedLeaseSelection, leaseBlockReason, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tryAcquireAcrossLocked(candidates)
}

type localAcrossCandidate struct {
	index     int
	candidate LeaseCandidateRequest
	active    int
	tokens    int64
}

func (c *localLeaseCoordinator) tryAcquireAcrossLocked(candidates []LeaseCandidateRequest) (CoordinatedLeaseSelection, leaseBlockReason, error) {
	feasible := make([]localAcrossCandidate, 0, len(candidates))
	blocked := leaseBlockConcurrency
	for index, candidate := range candidates {
		request := candidate.Request
		resourceBlocked := false
		for _, resource := range request.Resources {
			if resource.Limit > 0 && c.resources[resource.ID] >= resource.Limit {
				resourceBlocked = true
				break
			}
		}
		if resourceBlocked {
			continue
		}
		active, tokens := c.inflight[request.AccountID], c.tokens[request.AccountID]
		if tokenBudgetLimited(request.TokenBudget, request.Compaction, active, tokens, request.EstimatedTokens) {
			blocked = leaseBlockTokenBudget
			continue
		}
		feasible = append(feasible, localAcrossCandidate{index: index, candidate: candidate, active: active, tokens: tokens})
	}
	if len(feasible) == 0 {
		return CoordinatedLeaseSelection{}, blocked, nil
	}

	type targetState struct {
		choice   string
		idle     int
		active   int
		eligible int
		pending  int
		priority int
	}
	type membership struct {
		target    *targetState
		candidate localAcrossCandidate
	}
	type physicalAccount struct {
		id          string
		active      int
		memberships []*membership
		byChoice    map[string]*membership
		selected    *membership
	}
	targets := map[string]*targetState{}
	accounts := map[string]*physicalAccount{}
	for _, item := range feasible {
		choice := strings.TrimSpace(item.candidate.ChoiceKey)
		state := targets[choice]
		if state == nil {
			state = &targetState{choice: choice, pending: item.candidate.Pending, priority: item.candidate.BalancePriority}
			targets[choice] = state
		} else if item.candidate.Pending > state.pending {
			state.pending = item.candidate.Pending
		}
		if item.candidate.BalancePriority > state.priority {
			state.priority = item.candidate.BalancePriority
		}
		accountID := item.candidate.Request.AccountID
		account := accounts[accountID]
		if account == nil {
			account = &physicalAccount{id: accountID, active: item.active, byChoice: map[string]*membership{}}
			accounts[accountID] = account
		}
		member := account.byChoice[choice]
		if member == nil {
			member = &membership{target: state, candidate: item}
			account.byChoice[choice] = member
			account.memberships = append(account.memberships, member)
		} else if item.candidate.BalancePriority > member.candidate.candidate.BalancePriority ||
			(item.candidate.BalancePriority == member.candidate.candidate.BalancePriority && item.candidate.AccountScore < member.candidate.candidate.AccountScore) {
			member.candidate = item
		}
	}
	orderedTargets := make([]*targetState, 0, len(targets))
	for _, state := range targets {
		orderedTargets = append(orderedTargets, state)
	}
	sort.Slice(orderedTargets, func(i, j int) bool { return orderedTargets[i].choice < orderedTargets[j].choice })
	orderedAccounts := make([]*physicalAccount, 0, len(accounts))
	for _, account := range accounts {
		sort.Slice(account.memberships, func(i, j int) bool {
			return account.memberships[i].target.choice < account.memberships[j].target.choice
		})
		orderedAccounts = append(orderedAccounts, account)
	}
	sort.Slice(orderedAccounts, func(i, j int) bool { return orderedAccounts[i].id < orderedAccounts[j].id })
	c.rr++
	for _, state := range orderedTargets {
		// A target's priority must describe the memberships that remain after a
		// shared physical account is assigned to exactly one target for this
		// selection. Looking at every raw membership could prefer a target whose
		// only relief account was rotated to a different target.
		state.priority = 0
	}
	for index, account := range orderedAccounts {
		membershipIndex := int((c.rr - 1 + uint64(index)) % uint64(len(account.memberships)))
		account.selected = account.memberships[membershipIndex]
		state := account.selected.target
		if account.selected.candidate.candidate.BalancePriority > state.priority {
			state.priority = account.selected.candidate.candidate.BalancePriority
		}
		state.eligible++
		state.active += account.active
		if account.active == 0 {
			state.idle++
		}
	}
	bestTargets := make([]*targetState, 0, len(orderedTargets))
	targetMaxPriority := -2
	for _, state := range orderedTargets {
		if state.eligible == 0 {
			continue
		}
		if state.priority > targetMaxPriority {
			targetMaxPriority = state.priority
		}
	}
	if targetMaxPriority > 0 {
		for _, state := range orderedTargets {
			if state.eligible > 0 && state.priority == targetMaxPriority {
				bestTargets = append(bestTargets, state)
			}
		}
	} else {
		targetMaxPriority = 0
	}
	maxIdle := -1
	for _, state := range orderedTargets {
		if targetMaxPriority > 0 && state.priority != targetMaxPriority {
			continue
		}
		if state.eligible == 0 {
			continue
		}
		if state.idle > maxIdle {
			maxIdle = state.idle
			bestTargets = bestTargets[:0]
		}
		if state.idle == maxIdle {
			bestTargets = append(bestTargets, state)
		}
	}
	if maxIdle == 0 {
		bestTargets = bestTargets[:0]
		for _, state := range orderedTargets {
			if targetMaxPriority > 0 && state.priority != targetMaxPriority {
				continue
			}
			if state.eligible == 0 {
				continue
			}
			if len(bestTargets) == 0 {
				bestTargets = append(bestTargets, state)
				continue
			}
			best := bestTargets[0]
			left := int64(state.active+state.pending) * int64(best.eligible)
			right := int64(best.active+best.pending) * int64(state.eligible)
			if left < right {
				bestTargets = []*targetState{state}
			} else if left == right {
				bestTargets = append(bestTargets, state)
			}
		}
	}
	selectedTarget := bestTargets[int((c.rr-1)%uint64(len(bestTargets)))]

	choices := make([]*membership, 0, len(orderedAccounts))
	for _, account := range orderedAccounts {
		if account.selected.target == selectedTarget {
			choices = append(choices, account.selected)
		}
	}
	sort.SliceStable(choices, func(i, j int) bool {
		leftAccount := accounts[choices[i].candidate.candidate.Request.AccountID]
		rightAccount := accounts[choices[j].candidate.candidate.Request.AccountID]
		left, right := choices[i].candidate.candidate, choices[j].candidate.candidate
		if left.BalancePriority != right.BalancePriority {
			return left.BalancePriority > right.BalancePriority
		}
		if leftAccount.active != rightAccount.active {
			return leftAccount.active < rightAccount.active
		}
		if left.AccountScore != right.AccountScore {
			return left.AccountScore < right.AccountScore
		}
		return left.Request.AccountID < right.Request.AccountID
	})
	minActive := accounts[choices[0].candidate.candidate.Request.AccountID].active
	maxPriority := choices[0].candidate.candidate.BalancePriority
	minScore := choices[0].candidate.candidate.AccountScore
	tied := 0
	for tied < len(choices) && choices[tied].candidate.candidate.BalancePriority == maxPriority && accounts[choices[tied].candidate.candidate.Request.AccountID].active == minActive && choices[tied].candidate.candidate.AccountScore == minScore {
		tied++
	}
	selected := choices[int((c.rr-1)%uint64(maxInt(tied, 1)))].candidate
	request := selected.candidate.Request
	c.inflight[request.AccountID]++
	c.tokens[request.AccountID] += request.EstimatedTokens
	for _, resource := range request.Resources {
		c.resources[resource.ID]++
	}
	lease := &localCoordinatedLease{coordinator: c, request: request, fence: c.fence.Add(1)}
	return CoordinatedLeaseSelection{Lease: lease, ChoiceKey: selectedTarget.choice, AccountID: request.AccountID}, leaseBlockNone, nil
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

// redisLeaseAcquireAcrossScript evaluates all same-tier candidates and creates
// the winning lease in the same Redis execution.  Candidate account and
// resource keys are passed by index (rather than embedded in ARGV) so Redis has
// a complete declaration of every key touched by the script.
//
// Physical accounts that appear in more than one route choice are assigned to
// exactly one target for each selection.  The assignment rotates with the
// global Redis round-robin counter, preventing overlapping membership from
// manufacturing idle capacity while still allowing every valid membership to
// receive traffic.
const redisLeaseAcquireAcrossScript = `
if redis.call('GET', KEYS[1]) ~= ARGV[6] then return {-3, 0} end

local now = ARGV[1]
local lease_id = ARGV[2]
local channel = ARGV[3]
local candidate_count = tonumber(ARGV[4])
local position = 7
local candidates = {}
local cleaned_accounts = {}
local cleaned_resources = {}
local saw_token_block = false

for candidate_index = 1, candidate_count do
	local candidate = {
    index = candidate_index,
    choice = ARGV[position],
    account = ARGV[position + 1],
    requested = tonumber(ARGV[position + 2]),
    budget = tonumber(ARGV[position + 3]),
    compaction = ARGV[position + 4],
		score = tonumber(ARGV[position + 5]) or 0,
		priority = tonumber(ARGV[position + 6]) or 0,
		pending = tonumber(ARGV[position + 7]) or 0,
		ttl = tonumber(ARGV[position + 8]),
		account_key = tonumber(ARGV[position + 9]),
		tokens_key = tonumber(ARGV[position + 10]),
		resources = {}
	}
	local resource_count = tonumber(ARGV[position + 11])
	position = position + 12
  for resource_index = 1, resource_count do
    candidate.resources[resource_index] = {key = tonumber(ARGV[position]), limit = tonumber(ARGV[position + 1])}
    position = position + 2
  end

  if not cleaned_accounts[candidate.account_key] then
    local expired = redis.call('ZRANGEBYSCORE', KEYS[candidate.account_key], '-inf', now)
    if #expired > 0 then
      redis.call('ZREMRANGEBYSCORE', KEYS[candidate.account_key], '-inf', now)
      redis.call('HDEL', KEYS[candidate.tokens_key], unpack(expired))
    end
    cleaned_accounts[candidate.account_key] = true
  end
  candidate.active = redis.call('ZCARD', KEYS[candidate.account_key])
  candidate.tokens = 0
  for _, value in ipairs(redis.call('HVALS', KEYS[candidate.tokens_key])) do
    candidate.tokens = candidate.tokens + tonumber(value)
  end

  local resource_blocked = false
  for _, resource in ipairs(candidate.resources) do
    if not cleaned_resources[resource.key] then
      redis.call('ZREMRANGEBYSCORE', KEYS[resource.key], '-inf', now)
      cleaned_resources[resource.key] = true
    end
    if resource.limit > 0 and redis.call('ZCARD', KEYS[resource.key]) >= resource.limit then
      resource_blocked = true
    end
  end
  if not resource_blocked then
    if candidate.compaction == '0' and candidate.active > 0 and candidate.budget > 0 and candidate.tokens + candidate.requested > candidate.budget then
      saw_token_block = true
    else
      table.insert(candidates, candidate)
    end
  end
end

if #candidates == 0 then
  if saw_token_block then return {-2, 0} end
  return {-1, 0}
end

local rr = redis.call('INCR', KEYS[3])
local targets = {}
local target_by_choice = {}
local accounts = {}
local account_by_id = {}

for _, candidate in ipairs(candidates) do
  local target = target_by_choice[candidate.choice]
  if not target then
		target = {choice = candidate.choice, pending = candidate.pending, priority = candidate.priority, eligible = 0, idle = 0, active = 0}
    target_by_choice[candidate.choice] = target
    table.insert(targets, target)
	elseif candidate.pending > target.pending then
		target.pending = candidate.pending
	end
	if candidate.priority > target.priority then target.priority = candidate.priority end

  local account = account_by_id[candidate.account]
  if not account then
    account = {id = candidate.account, active = candidate.active, memberships = {}, membership_by_choice = {}}
    account_by_id[candidate.account] = account
    table.insert(accounts, account)
  end
  local membership = account.membership_by_choice[candidate.choice]
  if not membership then
    membership = {target = target, candidate = candidate}
    account.membership_by_choice[candidate.choice] = membership
    table.insert(account.memberships, membership)
	elseif candidate.priority > membership.candidate.priority or (candidate.priority == membership.candidate.priority and candidate.score < membership.candidate.score) then
		membership.candidate = candidate
  end
end

-- Count every physical account once globally, even when several choices refer
-- to it.  Rotating the owner avoids a lexical-choice bias for shared accounts.
for _, target in ipairs(targets) do
  -- Recompute from the memberships actually owned for this selection. A raw
  -- relief membership rotated away must not make its target win the priority
  -- tier without a selectable relief account.
  target.priority = 0
end
for account_index, account in ipairs(accounts) do
  local membership_index = ((rr + account_index - 2) % #account.memberships) + 1
  local membership = account.memberships[membership_index]
  account.selected_membership = membership
  local target = membership.target
  if membership.candidate.priority > target.priority then target.priority = membership.candidate.priority end
  target.eligible = target.eligible + 1
  target.active = target.active + account.active
  if account.active == 0 then target.idle = target.idle + 1 end
end

local best_targets = {}
local max_priority = 0
for _, target in ipairs(targets) do
  if target.eligible > 0 and target.priority > max_priority then max_priority = target.priority end
end
local max_idle = -1
for _, target in ipairs(targets) do
  if target.eligible > 0 and (max_priority == 0 or target.priority == max_priority) then
    if target.idle > max_idle then
      max_idle = target.idle
      best_targets = {target}
    elseif target.idle == max_idle then
      table.insert(best_targets, target)
    end
  end
end

if max_idle == 0 then
  best_targets = {}
  for _, target in ipairs(targets) do
    if target.eligible > 0 and (max_priority == 0 or target.priority == max_priority) then
      if #best_targets == 0 then
        best_targets = {target}
      else
        local best = best_targets[1]
        local left = (target.active + target.pending) * best.eligible
        local right = (best.active + best.pending) * target.eligible
        if left < right then
          best_targets = {target}
        elseif left == right then
          table.insert(best_targets, target)
        end
      end
    end
  end
end

local selected_target = best_targets[((rr - 1) % #best_targets) + 1]
local best_accounts = {}
local min_active = nil
local min_score = nil
local max_account_priority = nil
for _, account in ipairs(accounts) do
  local membership = account.selected_membership
  if membership.target == selected_target then
    local priority = membership.candidate.priority
    local score = membership.candidate.score
    if max_account_priority == nil or priority > max_account_priority or (priority == max_account_priority and (min_active == nil or account.active < min_active or (account.active == min_active and score < min_score))) then
      max_account_priority = priority
      min_active = account.active
      min_score = score
      best_accounts = {account}
    elseif priority == max_account_priority and account.active == min_active and score == min_score then
      table.insert(best_accounts, account)
    end
  end
end
local selected_account = best_accounts[((rr - 1) % #best_accounts) + 1]
local selected = selected_account.selected_membership.candidate
local expiry = tonumber(now) + selected.ttl
local fence = redis.call('INCR', KEYS[2])
redis.call('ZADD', KEYS[selected.account_key], expiry, lease_id)
redis.call('HSET', KEYS[selected.tokens_key], lease_id, selected.requested)
for _, resource in ipairs(selected.resources) do
  redis.call('ZADD', KEYS[resource.key], expiry, lease_id)
end
redis.call('HSET', KEYS[4], 'fence', fence, 'account', selected.account, 'choice', selected.choice, 'expires', expiry)
redis.call('PEXPIRE', KEYS[4], selected.ttl)
redis.call('PUBLISH', channel, lease_id)
return {fence, selected.index}
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

func (c *redisLeaseCoordinator) TryAcquireAcross(ctx context.Context, candidates []LeaseCandidateRequest) (CoordinatedLeaseSelection, leaseBlockReason, error) {
	if len(candidates) == 0 {
		return CoordinatedLeaseSelection{}, leaseBlockConcurrency, nil
	}

	normalized := append([]LeaseCandidateRequest(nil), candidates...)
	for index := range normalized {
		if normalized[index].Request.TTL <= 0 {
			normalized[index].Request.TTL = 20 * time.Minute
		}
	}

	now := time.Now()
	leaseID := c.nodeID + ":" + uuid.NewString()
	keys := []string{c.epochKey, c.prefix + "fence", c.prefix + "across:rr", c.prefix + "record:" + leaseKeyPart(leaseID)}
	keyIndexes := make(map[string]int)
	for index, key := range keys {
		keyIndexes[key] = index + 1 // Lua arrays are one-based.
	}
	addKey := func(key string) int {
		if existing := keyIndexes[key]; existing != 0 {
			return existing
		}
		keys = append(keys, key)
		index := len(keys)
		keyIndexes[key] = index
		return index
	}

	// ARGV[5] is reserved for format versioning so future script revisions can
	// add fields without making the epoch argument ambiguous.
	args := []interface{}{now.UnixMilli(), leaseID, c.channel, len(normalized), 1, c.epoch}
	for _, candidate := range normalized {
		request := candidate.Request
		accountPart := leaseKeyPart(request.AccountID)
		accountKey := addKey(c.prefix + "account:" + accountPart)
		tokensKey := addKey(c.prefix + "tokens:" + accountPart)
		score := candidate.AccountScore
		if score != score { // NaN must not enter Lua's comparison ordering.
			score = 0
		}
		args = append(args,
			strings.TrimSpace(candidate.ChoiceKey), request.AccountID, request.EstimatedTokens,
			request.TokenBudget, boolIntString(request.Compaction), strconv.FormatFloat(score, 'g', 17, 64),
			candidate.BalancePriority, candidate.Pending, request.TTL.Milliseconds(), accountKey, tokensKey, len(request.Resources),
		)
		for _, resource := range request.Resources {
			args = append(args, addKey(c.prefix+"resource:"+leaseKeyPart(resource.ID)), resource.Limit)
		}
	}

	result, err := c.client.Eval(ctx, redisLeaseAcquireAcrossScript, keys, args...).Slice()
	if err != nil {
		return CoordinatedLeaseSelection{}, leaseBlockCoordinator, fmt.Errorf("%w: Redis multi-candidate acquire: %v", ErrLeaseCoordinatorUnavailable, err)
	}
	if len(result) < 2 {
		return CoordinatedLeaseSelection{}, leaseBlockCoordinator, fmt.Errorf("%w: invalid Redis multi-candidate acquire result", ErrLeaseCoordinatorUnavailable)
	}
	code, codeErr := redisInt64(result[0])
	if codeErr != nil {
		return CoordinatedLeaseSelection{}, leaseBlockCoordinator, fmt.Errorf("%w: invalid Redis multi-candidate acquire result: %v", ErrLeaseCoordinatorUnavailable, codeErr)
	}
	switch code {
	case -1:
		return CoordinatedLeaseSelection{}, leaseBlockConcurrency, nil
	case -2:
		return CoordinatedLeaseSelection{}, leaseBlockTokenBudget, nil
	case -3:
		return CoordinatedLeaseSelection{}, leaseBlockCoordinator, fmt.Errorf("%w: Redis lease epoch changed; restart this node after Redis durability is restored", ErrLeaseCoordinatorUnavailable)
	}
	selectedIndex, indexErr := redisInt64(result[1])
	if indexErr != nil || selectedIndex < 1 || selectedIndex > int64(len(normalized)) {
		return CoordinatedLeaseSelection{}, leaseBlockCoordinator, fmt.Errorf("%w: Redis selected invalid multi-candidate index %v", ErrLeaseCoordinatorUnavailable, result[1])
	}
	selected := normalized[selectedIndex-1]
	request := selected.Request
	accountPart := leaseKeyPart(request.AccountID)
	leaseKeys := []string{
		c.prefix + "account:" + accountPart,
		c.prefix + "tokens:" + accountPart,
		c.prefix + "record:" + leaseKeyPart(leaseID),
		c.epochKey,
	}
	for _, resource := range request.Resources {
		leaseKeys = append(leaseKeys, c.prefix+"resource:"+leaseKeyPart(resource.ID))
	}
	lease := &redisCoordinatedLease{coordinator: c, request: request, id: leaseID, fence: uint64(code), keys: leaseKeys, ttl: request.TTL, stop: make(chan struct{})}
	go lease.renewLoop()
	return CoordinatedLeaseSelection{Lease: lease, ChoiceKey: selected.ChoiceKey, AccountID: request.AccountID}, leaseBlockNone, nil
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
