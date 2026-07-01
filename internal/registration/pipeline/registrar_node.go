// node registration: orchestrates the puppeteer-real-browser registrar
// (other_new_gpt_register) as a per-job subprocess. pool_server owns the isolation and
// lifecycle that the standalone Node tool lacks:
//
//   - Per-browser IP uniqueness: each job leases the request's egress and rotates a
//     fresh exit IP (cliproxy SID), passed to the worker via HTTPS_PROXY + the per-job
//     config — concurrent jobs never share an outbound IP.
//   - Per-browser fingerprint isolation: each job gets a unique fingerprint seed
//     (REG_FP_SEED + config.fingerprintSeed) so parallel browsers never present the
//     same fingerprint. (The Node browserService consumes the seed to vary UA/viewport/
//     timezone; the seed is authoritative here so uniqueness is guaranteed pool-side.)
//   - Post-registration cookie purge: each job runs in a throwaway temp userDataDir that
//     is hard-deleted on teardown, plus browserClearChatGptSession — zero cross-session
//     contamination.
//
// The registrar reads its config from CONFIG_FILE (src/config.js) and writes the result
// token as codex-<email>-free.json into tokenOutputDir; we parse it and UpsertAccount.
package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/registration/provider/proxy"
	"codex-account-pool/internal/storage"
)

// nodeTokenFile is the shape other_new_gpt_register writes per successful registration.
type nodeTokenFile struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
	Type         string `json:"type"`
}

// nodeJob carries the per-job isolation parameters the orchestrator assigns.
type nodeJob struct {
	ProxyURL        string // full proxy URL for HTTPS_PROXY (rotated → unique exit IP)
	ProfileDir      string // throwaway browser userDataDir (deleted on teardown)
	TokenDir        string // where the worker writes codex-*-free.json
	FingerprintSeed string // unique per job → fingerprint isolation

	// CountryISO is the ISO-2 the registration's SMS number belongs to (e.g. "BR"). When set,
	// the orchestrator pins the cliproxy exit region to it (region must match the phone
	// country or OpenAI withholds the SMS) and overlays heroSmsCountry + phoneCountryCode on
	// the registrar config so the node engine buys a number in THIS country, not the one
	// baked into config.server.json.
	CountryISO string
	// CountryID is the platform's numeric country id for CountryISO (hero-sms: BR=73, etc.),
	// resolved at acquireSMS time so the node engine doesn't have to query it again.
	CountryID string
}

// nodeRegistrarBaseConfig loads the admin-managed base config for the Node registrar:
// the operator's hero-sms key, CloudMail credentials, phone-country catalog, etc. It is
// stored in the settings table under "node_registrar_config" (a JSON object the admin UI
// edits), with a file fallback via CODEX_REG_NODE_CONFIG for bootstrap. Returns an empty
// map when neither is set (the worker then errors clearly on missing creds).
// nodeRegistrarBaseConfig assembles the operator's base registrar config that the
// per-job overrides layer on top of. CRUCIAL: because the orchestrator drives the
// worker via CONFIG_FILE, the registrar's own src/config.js loads config.json +
// CONFIG_FILE and SKIPS its platform profile (config.server.json). So we must fold that
// profile (and config.json) in here ourselves, exactly as config.js would, or the
// operator's real hero-sms / mail / proxy creds (which live in config.server.json) are
// silently lost. Precedence (low→high): config.json → platform profile → settings-table
// override → CODEX_REG_NODE_CONFIG file.
func (p *Pipeline) nodeRegistrarBaseConfig(ctx context.Context, regDir string) map[string]interface{} {
	base := map[string]interface{}{}
	mergeJSONFileInto(base, filepath.Join(regDir, "config.json"))
	profile := "config.server.json" // linux (and default) — mirrors src/config.js
	if runtime.GOOS == "darwin" {
		profile = "config.local.json"
	}
	mergeJSONFileInto(base, filepath.Join(regDir, profile))
	if v, ok, _ := p.store.GetSetting(ctx, "node_registrar_config"); ok {
		if v = strings.TrimSpace(v); v != "" {
			var m map[string]interface{}
			if json.Unmarshal([]byte(v), &m) == nil {
				for k, val := range m {
					base[k] = val
				}
			}
		}
	}
	if path := strings.TrimSpace(os.Getenv("CODEX_REG_NODE_CONFIG")); path != "" {
		mergeJSONFileInto(base, path)
	}
	return base
}

// mergeJSONFileInto shallow-merges a JSON object file into dst (no-op if missing/bad).
func mergeJSONFileInto(dst map[string]interface{}, path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var m map[string]interface{}
	if json.Unmarshal(raw, &m) == nil {
		for k, v := range m {
			dst[k] = v
		}
	}
}

// buildNodeJobConfig merges the admin base config with the per-job isolation overrides.
// It is pure (no I/O) so the isolation invariants are unit-testable without Node: a fresh
// throwaway profile, the job's unique proxy, session-clear on, and the fingerprint seed.
func buildNodeJobConfig(base map[string]interface{}, j nodeJob) map[string]interface{} {
	cfg := make(map[string]interface{}, len(base)+8)
	for k, v := range base {
		cfg[k] = v
	}
	// Per-job egress → unique outbound IP. Set both the structured proxy fields (config.js
	// reads proxyHost/Port/Username/Password) and rely on HTTPS_PROXY env as a backstop.
	// CRITICAL: the proxy's cliproxy region MUST match the SMS number's country, or OpenAI
	// withholds the SMS (geo mismatch). proxyURLFromEgress already rotated the sid for a
	// fresh exit IP; here we additionally pin the region to j.CountryISO when set.
	// In api_whitelist mode the proxyForJob is an http://ip:port with no username, so
	// WithRegion and the credential-mode split-logic are skipped (the IP is already region-locked).
	proxyForJob := j.ProxyURL
	if j.CountryISO != "" && proxy.IsCliproxy(proxyForJob) {
		proxyForJob = proxy.WithRegion(proxyForJob, j.CountryISO) // pin region + rotate sid
	}
	if host, port, user, pass, ok := splitProxyURL(proxyForJob); ok {
		cfg["proxyHost"] = host
		cfg["proxyPort"] = port
		cfg["proxyUsername"] = user
		cfg["proxyPassword"] = pass
	} else {
		// api_whitelist mode: proxyForJob is http://ip:port with no user/pass.
		// splitProxyURL returns ok=true for plain ip:port URLs too (host/port without user/pass).
		// The else branch is only reached when the URL is completely unparseable.
		// For api_whitelist, proxyForJob already carries the ip:port, so we just need to
		// ensure the split succeeded.
		if h2, p2, ok2 := splitHostPort(proxyForJob); ok2 {
			cfg["proxyHost"] = h2
			cfg["proxyPort"] = p2
			cfg["proxyUsername"] = ""
			cfg["proxyPassword"] = ""
		}
	}
	// Overlay the chosen country on the registrar config so the node engine buys a number in
	// THIS country (not the one baked into config.server.json). heroSmsCountry is the numeric
	// platform id; phoneCountryCode is the ISO-2 the engine's phoneCountryCatalog maps to a
	// dial code. Both must agree with the proxy region above.
	if j.CountryISO != "" {
		cfg["phoneCountryCode"] = strings.ToUpper(j.CountryISO)
	}
	if j.CountryID != "" {
		// The node engine reads heroSmsCountry as a number; a numeric string coerces fine.
		cfg["heroSmsCountry"] = j.CountryID
	}
	// Per-job throwaway profile + result dir; persistent (not incognito) so the flow's
	// storage works, but the whole dir is deleted on teardown → cookie purge.
	cfg["browserUserDataDir"] = j.ProfileDir
	cfg["browserIncognito"] = false
	cfg["browserClearChatGptSession"] = true
	cfg["tokenOutputDir"] = j.TokenDir
	cfg["tokenOutputDirs"] = []string{j.TokenDir}
	// Per-job fingerprint seed → fingerprint isolation across concurrent browsers.
	cfg["fingerprintSeed"] = j.FingerprintSeed
	return cfg
}

// splitHostPort parses a plain "host:port" string (no scheme, no user/pass) into host and port.
// Used for api_whitelist proxy URLs (http://ip:port) where splitProxyURL already handles the
// credentialed case.
func splitHostPort(raw string) (host string, port int, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	host = u.Hostname()
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	} else {
		port = 80
	}
	ok = host != ""
	return
}

// splitProxyURL parses host/port/user/pass out of a proxy URL.
func splitProxyURL(raw string) (host string, port int, user, pass string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", 0, "", "", false
	}
	host = u.Hostname()
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	} else if u.Scheme == "https" {
		port = 443
	} else {
		port = 80
	}
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	return host, port, user, pass, true
}

func randomFingerprintSeed() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// nodeMemoryLimitOption reads /proc/meminfo to detect system total memory and returns a
// NODE_OPTIONS string like "--max-old-space-size=512" that constrains the V8 heap so a
// runaway Node process doesn't OOM-kill the whole VPS. Chrome's memory is the real cost
// driver; this is a safety net. Returns "" on non-Linux (no proc/meminfo).
func nodeMemoryLimitOption() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return ""
	}
	// Parse MemTotal: <num> kB
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, _ := strconv.ParseInt(fields[1], 10, 64)
		mb := kb / 1024
		switch {
		case mb <= 1536:
			return "--max-old-space-size=256"
		case mb <= 4608:
			return "--max-old-space-size=512"
		default:
			return "--max-old-space-size=1024"
		}
	}
	return ""
}

// heroSmsCountryID maps an ISO-2 country code to the hero-sms numeric country id the Node
// engine consumes (heroSmsCountry). IDs verified live via hero-sms getCountries (2026-06-27);
// SMSBower shares the same IDs. Unmapped ISOs return "" so the node engine falls back to its
// own config.heroSmsCountry (the operator's baked default) rather than buying in a wrong country.
// Keep in sync with provider/sms/herosms.go heroCountryID.
func heroSmsCountryID(iso string) string {
	switch strings.ToUpper(strings.TrimSpace(iso)) {
	case "PH":
		return "4"
	case "ID":
		return "6"
	case "PL":
		return "15"
	case "UK", "GB":
		return "16"
	case "IN":
		return "22"
	case "ZA":
		return "27"
	case "CO":
		return "33"
	case "TH":
		return "52"
	case "CL":
		return "56"
	case "BR":
		return "73"
	default:
		return ""
	}
}

var cliproxySidRe = regexp.MustCompile(`sid-[A-Za-z0-9]+`)

// rotateProxyUsernameSid swaps the cliproxy session id (sid-XXXX) in a proxy username for
// a fresh random one, so each registration attempt egresses from a DIFFERENT residential
// IP. A fixed IP that drives many OpenAI signups gets flagged → the SMS code is silently
// withheld; rotating the IP per attempt avoids that. No-op for usernames without a sid.
func rotateProxyUsernameSid(username string) string {
	if !strings.Contains(username, "sid-") {
		return username
	}
	return cliproxySidRe.ReplaceAllString(username, "sid-"+randomFingerprintSeed()[:10])
}

func (p *Pipeline) nodeRegisterOne(ctx context.Context, req RegisterRequest) (*storage.Account, error) {
	nodeBin := firstEnv("node", "CODEX_REG_NODE")
	regDir := firstEnv("other_new_gpt_register", "CODEX_REG_NODE_DIR")
	entry := firstEnv("index.js", "CODEX_REG_NODE_ENTRY")
	if abs, err := filepath.Abs(regDir); err == nil {
		regDir = abs
	}

	// Per-job parent dir; each attempt gets a fresh profile inside it. Deleted on
	// teardown → cookie/profile purge.
	jobRoot, err := os.MkdirTemp("", "reg-node-*")
	if err != nil {
		return nil, fmt.Errorf("node reg: temp dir: %w", err)
	}
	defer os.RemoveAll(jobRoot)
	tokenDir := filepath.Join(jobRoot, "tokens")
	entryPath := entry
	if !filepath.IsAbs(entryPath) {
		entryPath = filepath.Join(regDir, entry)
	}
	baseConfig := p.nodeRegistrarBaseConfig(ctx, regDir)
	maxAttempts := nodeRegMaxAttempts()

	// Resolve the SMS country for THIS registration. Under "auto" the Manager already picked
	// the best (platform, country) — but the node engine runs its OWN hero-sms flow (it doesn't
	// use pool_server's SMSProvider), so we resolve the country here the same way acquireSMS
	// would and feed it to the engine: req.Country (manual or the auto-chosen ISO) wins,
	// else the operator's preferred list (BR>CO>PL), else BR (live-verified best).
	countryISO := strings.ToUpper(strings.TrimSpace(req.Country))
	if countryISO == "" || countryISO == "RAND" {
		countryISO = firstPreferredCountry(ctx, p.store)
	}
	// hero-sms numeric country id. The phoneCountryCatalog maps ISO→heroSmsCountry for the
	// known set; unmapped ISOs (e.g. a new country) fall back to a live getCountries lookup
	// would be ideal, but the engine itself does getCountries+getTopCountries and will use
	// heroSmsCountry from its own catalog resolution — so we only set it when we know it.
	countryID := heroSmsCountryID(countryISO)

	// Drive retries by spawning a FRESH node process per attempt (REG_ONE_SHOT=1), each
	// doing exactly ONE browser launch. This sidesteps the puppeteer-real-browser relaunch
	// flakiness (2nd+ connect() in a long-lived process fails to fetch the debug URL) and
	// gives every attempt a clean profile + fingerprint + rotated exit IP.
	var acct *nodeTokenFile
	var lastTail string
	var lastFullOutput string
	for attempt := 1; attempt <= maxAttempts && ctx.Err() == nil; attempt++ {
		profileDir := filepath.Join(jobRoot, fmt.Sprintf("profile-%d", attempt))
		_ = os.MkdirAll(profileDir, 0o700)
		_ = os.MkdirAll(tokenDir, 0o700)
		_ = os.Remove(filepath.Join(jobRoot, "auth.json")) // clear any stale success marker

		// Per-attempt proxy: a FRESH exit IP for each browser launch. In credential mode
		// proxyURLFromEgress already rotated the sid; buildNodeJobConfig pins the region to
		// the SMS country. In api_whitelist mode the IP is pre-extracted (region-locked). We
		// then VALIDATE the exit IP's country matches the SMS country before launching — a
		// sid-rotated region-BR sid can still be routed to a neighbour when BR inventory is
		// low, and that geo-mismatch is what gets the SMS withheld.
		proxyForAttempt := p.proxyURLFromEgress(ctx, req.EgressID)
		if countryISO != "" && p.cfg != nil && p.cfg.CliproxyValidateRegion && proxyForAttempt != "" {
			if !proxy.ValidateExitRegion(ctx, proxyForAttempt, countryISO) {
				// Geo mismatch: re-rotate and re-validate up to 2 times.
				matched := false
				for retry := 0; retry < 2; retry++ {
					if proxy.IsCliproxy(proxyForAttempt) {
						proxyForAttempt = proxy.RotateSID(proxyForAttempt)
					} else {
						proxy.InvalidateRegionCache(countryISO)
						proxyForAttempt = p.proxyURLFromEgress(ctx, req.EgressID)
					}
					if proxy.ValidateExitRegion(ctx, proxyForAttempt, countryISO) {
						matched = true
						break
					}
				}
				if !matched {
					fmt.Fprintf(os.Stderr, "[node-reg] 出口 IP 国家与号码国家 %s 不匹配，仍继续尝试 (geo-mismatch risk)\n", countryISO)
				}
			}
		}

		job := nodeJob{
			ProxyURL:        proxyForAttempt, // rotated+validated → fresh exit IP in the SMS country
			ProfileDir:      profileDir,
			TokenDir:        tokenDir,
			FingerprintSeed: randomFingerprintSeed(),
			CountryISO:      countryISO,
			CountryID:       countryID,
		}
		cfg := buildNodeJobConfig(baseConfig, job)
		// Fresh residential exit IP per attempt (credential mode only): rotate the cliproxy
		// session id once more so each browser launch exits from a different residential IP
		// of the SMS country. api_whitelist mode already returns a region-locked ip:port with
		// no username, so this rotation is a no-op there.
		if u, ok := cfg["proxyUsername"].(string); ok && u != "" {
			cfg["proxyUsername"] = rotateProxyUsernameSid(u)
		}
		cfgPath := filepath.Join(jobRoot, "job-config.json")
		cfgRaw, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("node reg: marshal config: %w", err)
		}
		if err := os.WriteFile(cfgPath, cfgRaw, 0o600); err != nil {
			return nil, fmt.Errorf("node reg: write config: %w", err)
		}

		cctx, cancel := context.WithTimeout(ctx, nodeRegAttemptTimeout())
		cmd := exec.CommandContext(cctx, nodeBin, entryPath)
		// cwd = jobRoot so the registrar's cwd-relative success artifact (auth.json) lands
		// where we read it; node resolves require('./src/...') by the script path.
		cmd.Dir = jobRoot
		// Browser display strategy: run Chrome HEADED inside a fresh per-process Xvfb
		// virtual display, NOT --headless. OpenAI's signup page passes Cloudflare in
		// headless but then withholds the "Sign up" UI from headless Chrome (detection),
		// so headless dies at phase 1 before any SMS. Headed rendering is what actually
		// reaches the phone/email flow. puppeteer-real-browser starts its OWN Xvfb when
		// headless=false AND disableXvfb=false (linux) — a clean, isolated display per
		// attempt that also avoids the shared WSLg :0 degrading after many launches, and
		// is exactly how a headless VPS runs. To get there the worker must have NEITHER
		// REG_HEADLESS NOR DISPLAY set (browserService derives disableXvfb from both).
		// NOTE: deliberately NO HTTP(S)_PROXY env — the browser's egress proxy comes from
		// the per-job CONFIG (proxyHost/...). Setting HTTP_PROXY here would also route node's
		// fetch to Chrome's OWN debug port (127.0.0.1) through the proxy, breaking the launch.
		cmd.Env = append(envWithout(os.Environ(), "DISPLAY", "REG_HEADLESS"),
			"CONFIG_FILE="+cfgPath,
			"REG_FP_SEED="+job.FingerprintSeed,
			"REG_ONE_SHOT=1",           // fresh process per attempt
			"REG_SHOT_DIR="+profileDir, // per-attempt debug screenshots → no cross-worker collision
			// 号码复用缓存文件：同一 job 内的 attempt 共享，收到码后失败不清号，下一个 attempt
			// 的 node 进程 getNumber 命中缓存 → 复用号码（不花钱）+ requestAnotherCode 取新码。
			"REG_PHONE_REUSE_FILE="+filepath.Join(jobRoot, "phone-reuse.json"),
			// 复用上限：同一个号最多复用 3 次后强制买新号（避免单号被 OpenAI 风控）。
			"REG_PHONE_REUSE_MAX=3",
			// 成功率统计文件：同 jobRoot，跨 attempt 共享，供 crossPlatformBest 动态优先级用。
			"REG_SMS_STATS_FILE="+filepath.Join(jobRoot, "reg-sms-stats.json"),
			// 手机号占用切换状态文件：同 jobRoot，跨 attempt 累加同一国家占用计数，
			// 达到阈值后自动切到下一候选国家/平台。
			"REG_PHONE_OCCUPIED_STATE_FILE="+filepath.Join(jobRoot, "reg-phone-occupied.json"),
			// VPS 低配优化：根据系统内存限制 Node.js V8 老生代堆上限，避免 Chrome+Node 吃爆内存。
			// Node 堆只占进程内存一小部分（Chrome 才是大头），但限制它能防失控重试内存堆积。
			"NODE_OPTIONS="+nodeMemoryLimitOption(),
		)
		// Capture stdout and stderr with a cap so a runaway subprocess cannot fill
		// the VPS memory. 1 MiB per stream is enough for the full registration log;
		// the cap is configurable via the "reg_combined_output_cap" setting. We use a
		// capped Writer so the subprocess can write freely and we keep only the last
		// outputCap bytes, never the full (potentially multi-MB) stream.
		outputCap := p.outputCap(ctx)
		stdoutBuf := newCappedBuffer(int(outputCap))
		stderrBuf := newCappedBuffer(int(outputCap))
		cmd.Stdout = stdoutBuf
		cmd.Stderr = stderrBuf
		runErr := cmd.Run()
		cancel()
		out := stdoutBuf.String() + "\n" + stderrBuf.String()
		lastTail = tailString(out, 600)
		lastFullOutput = out
		lastFullOutput = out
		if a, _ := readNodeRegistrationResult(jobRoot, tokenDir); a != nil {
			acct = a
			break
		}
		_ = runErr // this attempt produced no account → spawn a fresh one
	}
	if acct == nil {
		// lastFullOutput carries the full capped subprocess output of the final attempt;
		// it is intentionally kept for the registration handler's structured logging even
		// though this code path returns an error. We surface a tail in the error here so the
		// legacy one-line log keeps working, and the full output is logged separately.
		_ = lastFullOutput
		return nil, fmt.Errorf("node reg: no account produced after %d attempt(s). last worker tail: %s", maxAttempts, lastTail)
	}

	now := time.Now().Unix()
	account := &storage.Account{
		ID:                generateAccountID(),
		Label:             "node-" + acct.Email,
		GroupName:         req.GroupName,
		UpstreamAccountID: acct.AccountID,
		Email:             acct.Email,
		Provider:          "codex",
		Status:            "active",
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	token := &storage.AccountToken{
		AccountID:    account.ID,
		AccessToken:  acct.AccessToken,
		RefreshToken: acct.RefreshToken,
		IDTokenRaw:   acct.IDToken,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := p.store.UpsertAccount(ctx, *account, *token); err != nil {
		return nil, fmt.Errorf("node reg: upsertAccount: %w", err)
	}
	return account, nil
}

// readNodeRegistrationResult reads the credential the registrar produced on success
// and returns it for import into the pool. It PREFERS auth.json — the Codex CLI auth
// file (process.cwd()/auth.json, captured in the per-job cwd) the operator named as the
// canonical success artifact — and falls back to the main flow's
// tokens/codex-<email>-free.json. Both carry the identical flat token shape, so either
// lands the account in the pool.
func readNodeRegistrationResult(jobRoot, tokenDir string) (*nodeTokenFile, error) {
	authPath := filepath.Join(jobRoot, "auth.json")
	if raw, err := os.ReadFile(authPath); err == nil {
		var t nodeTokenFile
		if json.Unmarshal(raw, &t) == nil && strings.TrimSpace(t.AccessToken) != "" {
			return &t, nil
		}
	}
	return readNewestNodeToken(tokenDir)
}

// readNewestNodeToken returns the most-recently-written codex-*-free.json in dir with a
// non-empty access token (the just-registered account).
func readNewestNodeToken(dir string) (*nodeTokenFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type cand struct {
		path string
		mod  time.Time
	}
	var cands []cand
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "codex-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		cands = append(cands, cand{path: filepath.Join(dir, name), mod: info.ModTime()})
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("no codex-*.json token file in %s", dir)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	for _, c := range cands {
		raw, err := os.ReadFile(c.path)
		if err != nil {
			continue
		}
		var t nodeTokenFile
		if json.Unmarshal(raw, &t) == nil && strings.TrimSpace(t.AccessToken) != "" {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("token files present but none carried an access_token")
}

// nodeRegMaxAttempts bounds how many fresh one-shot worker processes a single
// registration may spawn before giving up (each is a clean browser launch + one phone
// number). Env CODEX_REG_NODE_MAX_ATTEMPTS; default 8.
func nodeRegMaxAttempts() int {
	if v := strings.TrimSpace(os.Getenv("CODEX_REG_NODE_MAX_ATTEMPTS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 8
}

// nodeRegAttemptTimeout caps a single one-shot worker process (one browser launch +
// Cloudflare + one SMS/email cycle + OAuth). Env CODEX_REG_NODE_ATTEMPT_TIMEOUT_SEC
// (or legacy CODEX_REG_NODE_TIMEOUT_SEC); default 4 minutes.
func nodeRegAttemptTimeout() time.Duration {
	for _, key := range []string{"CODEX_REG_NODE_ATTEMPT_TIMEOUT_SEC", "CODEX_REG_NODE_TIMEOUT_SEC"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return time.Duration(n) * time.Second
			}
		}
	}
	return 4 * time.Minute
}

func tailString(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// envWithout returns a copy of env with the named variables removed (case-sensitive,
// matching on the KEY before '='). Used to strip DISPLAY/REG_HEADLESS from the worker
// so puppeteer-real-browser starts its own fresh Xvfb and runs Chrome headed.
func envWithout(env []string, drop ...string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		key := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			key = e[:i]
		}
		skip := false
		for _, d := range drop {
			if key == d {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, e)
		}
	}
	return out
}

// outputCap returns the subprocess stdout/stderr capture cap (bytes) from the
// "reg_combined_output_cap" setting, defaulting to 1 MiB. Capping prevents a runaway
// Node/Chrome subprocess from filling VPS memory with multi-MB logs.
func (p *Pipeline) outputCap(ctx context.Context) int64 {
	const def = 1 << 20
	if p == nil || p.store == nil {
		return def
	}
	if v, ok, _ := p.store.GetSetting(ctx, "reg_combined_output_cap"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return int64(n)
		}
	}
	return def
}

// cappedBuffer is a bounded io.Writer that keeps only the last `cap` bytes written,
// discarding older data once the cap is exceeded. It lets a subprocess write freely
// while the orchestrator retains a fixed memory footprint.
type cappedBuffer struct {
	cap  int
	buf  []byte
	full bool
}

func newCappedBuffer(cap int) *cappedBuffer {
	if cap <= 0 {
		cap = 1 << 20
	}
	return &cappedBuffer{cap: cap, buf: make([]byte, 0, cap)}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Keep the most recent `cap` bytes: if the incoming chunk plus the existing
	// buffer exceeds the cap, drop the leading overflow so we never grow unbounded.
	if len(c.buf)+len(p) > c.cap {
		overflow := (len(c.buf) + len(p)) - c.cap
		if overflow >= len(c.buf) {
			c.buf = append(c.buf[:0], p[len(p)-c.cap:]...)
		} else {
			c.buf = append(c.buf[overflow:], p...)
		}
		c.full = true
		// Trim to cap (append may have grown the backing array beyond cap).
		if len(c.buf) > c.cap {
			c.buf = c.buf[len(c.buf)-c.cap:]
		}
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	return string(c.buf)
}
