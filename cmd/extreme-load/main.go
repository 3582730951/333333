package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	tiktoken "github.com/pkoukk/tiktoken-go"
	"golang.org/x/net/http2"
)

const fixtureManifestVersion = 1

const (
	o200kBaseURL    = "https://openaipublic.blob.core.windows.net/encodings/o200k_base.tiktoken"
	o200kBaseSHA256 = "446a9538cb6c348e3516120d7c08b09f57c36495e2acfffe59a5bf8b0cfb1a2d"
)

type fixtureManifest struct {
	Version      int            `json:"version"`
	Model        string         `json:"model"`
	Encoding     string         `json:"encoding"`
	EncodingSHA  string         `json:"encoding_sha256"`
	TargetTokens int            `json:"target_tokens"`
	Tolerance    float64        `json:"tolerance"`
	Fixtures     []fixtureEntry `json:"fixtures"`
}

type fixtureEntry struct {
	File             string  `json:"file"`
	Bytes            int64   `json:"bytes"`
	Tokens           int     `json:"tokens"`
	SHA256           string  `json:"sha256"`
	GzipToRawRatio   float64 `json:"gzip_to_raw_ratio"`
	PromptCacheKey   string  `json:"prompt_cache_key"`
	VerificationKind string  `json:"verification_kind"`
}

type loadResult struct {
	Requests           int     `json:"requests"`
	Succeeded          int     `json:"succeeded"`
	Failed             int     `json:"failed"`
	TargetRPS          int     `json:"target_rps"`
	MinimumAchievedRPS float64 `json:"minimum_achieved_rps"`
	AchievedRPS        float64 `json:"achieved_rps"`
	ElapsedSeconds     float64 `json:"elapsed_seconds"`
	P50Millis          float64 `json:"p50_millis"`
	P95Millis          float64 `json:"p95_millis"`
	P99Millis          float64 `json:"p99_millis"`
	BytesSent          int64   `json:"bytes_sent"`
	VerifiedFixtures   int     `json:"verified_fixtures"`
	VerifiedMinTokens  int     `json:"verified_min_tokens"`
	VerifiedMaxTokens  int     `json:"verified_max_tokens"`
}

func main() {
	tiktoken.SetBpeLoader(&verifiedBPELoader{fallback: tiktoken.NewDefaultBpeLoader()})
	if len(os.Args) < 2 {
		log.Fatal("usage: extreme-load generate|verify|mock|seed|run [flags]")
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = generateCommand(os.Args[2:])
	case "verify":
		err = verifyCommand(os.Args[2:])
	case "mock":
		err = mockCommand(os.Args[2:])
	case "seed":
		err = seedCommand(os.Args[2:])
	case "run":
		err = runCommand(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}

type verifiedBPELoader struct {
	fallback tiktoken.BpeLoader
}

func (l *verifiedBPELoader) LoadTiktokenBpe(source string) (map[string]int, error) {
	if source != o200kBaseURL {
		return l.fallback.LoadTiktokenBpe(source)
	}
	cacheDir := firstNonEmptyString(os.Getenv("TIKTOKEN_CACHE_DIR"), os.Getenv("DATA_GYM_CACHE_DIR"), filepath.Join(os.TempDir(), "data-gym-cache"))
	cacheKey := fmt.Sprintf("%x", sha1.Sum([]byte(source)))
	cachePath := filepath.Join(cacheDir, cacheKey)
	raw, readErr := os.ReadFile(cachePath)
	if readErr == nil && verifiedSHA256(raw, o200kBaseSHA256) {
		return parseTiktokenBPE(raw)
	}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	response, err := (&http.Client{Timeout: 2 * time.Minute}).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tokenizer download returned HTTP %d", response.StatusCode)
	}
	raw, err = io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if !verifiedSHA256(raw, o200kBaseSHA256) {
		sum := sha256.Sum256(raw)
		return nil, fmt.Errorf("o200k_base checksum mismatch: got %x", sum)
	}
	if err = os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, err
	}
	temp, err := os.CreateTemp(cacheDir, cacheKey+".*.tmp")
	if err != nil {
		return nil, err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err = temp.Chmod(0o600); err == nil {
		_, err = temp.Write(raw)
	}
	if err == nil {
		err = temp.Sync()
	}
	err = errors.Join(err, temp.Close())
	if err != nil {
		return nil, err
	}
	if err = os.Rename(tempPath, cachePath); err != nil {
		return nil, err
	}
	return parseTiktokenBPE(raw)
}

func verifiedSHA256(raw []byte, expected string) bool {
	sum := sha256.Sum256(raw)
	return strings.EqualFold(hex.EncodeToString(sum[:]), expected)
}

func parseTiktokenBPE(raw []byte) (map[string]int, error) {
	ranks := make(map[string]int, 200_000)
	for lineNumber, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		parts := bytes.Fields(line)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid tokenizer row %d", lineNumber+1)
		}
		token, err := base64.StdEncoding.DecodeString(string(parts[0]))
		if err != nil {
			return nil, fmt.Errorf("decode tokenizer row %d: %w", lineNumber+1, err)
		}
		rank, err := strconv.Atoi(string(parts[1]))
		if err != nil || rank < 0 {
			return nil, fmt.Errorf("invalid tokenizer rank on row %d", lineNumber+1)
		}
		ranks[string(token)] = rank
	}
	if len(ranks) == 0 {
		return nil, errors.New("tokenizer contains no ranks")
	}
	return ranks, nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func generateCommand(args []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	dir := flags.String("dir", "", "empty output directory")
	count := flags.Int("count", 256, "fixture count")
	target := flags.Int("tokens", 1_000_000, "target tokens per fixture")
	tolerance := flags.Float64("tolerance", 0.005, "allowed relative token error")
	model := flags.String("model", "gpt-5.6-sol", "target model recorded in the manifest")
	encodingName := flags.String("encoding", "o200k_base", "exact tokenizer encoding")
	workers := flags.Int("workers", runtime.NumCPU(), "parallel fixture generators")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *count < 1 || *target < 1 || *tolerance <= 0 || *tolerance > 0.1 || *workers < 1 {
		return errors.New("dir, positive count/tokens/workers and tolerance in (0,0.1] are required")
	}
	if err := ensureEmptyDirectory(*dir); err != nil {
		return err
	}
	encoding, err := tiktoken.GetEncoding(*encodingName)
	if err != nil {
		return fmt.Errorf("load exact tokenizer %s: %w", *encodingName, err)
	}
	manifest := fixtureManifest{Version: fixtureManifestVersion, Model: *model, Encoding: *encodingName, EncodingSHA: tokenizerChecksum(*encodingName), TargetTokens: *target, Tolerance: *tolerance, Fixtures: make([]fixtureEntry, *count)}
	type generatedFixture struct {
		index int
		entry fixtureEntry
		err   error
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jobs := make(chan int)
	results := make(chan generatedFixture, min(*workers, *count))
	var group sync.WaitGroup
	for worker := 0; worker < min(*workers, *count); worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			defer supervisor.Recover("extreme-load-fixture-generator")
			for {
				select {
				case <-ctx.Done():
					return
				case index, ok := <-jobs:
					if !ok {
						return
					}
					entry, err := generateFixtureEntry(encoding, *dir, *model, *encodingName, index, *target, *tolerance)
					select {
					case results <- generatedFixture{index: index, entry: entry, err: err}:
					case <-ctx.Done():
						return
					}
					if err != nil {
						return
					}
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		defer supervisor.Recover("extreme-load-fixture-jobs")
		for index := 0; index < *count; index++ {
			select {
			case jobs <- index:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		defer close(results)
		defer supervisor.Recover("extreme-load-fixture-results")
		group.Wait()
	}()
	var firstErr error
	completed := 0
	for result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("fixture %d: %w", result.index, result.err)
				cancel()
			}
			continue
		}
		manifest.Fixtures[result.index] = result.entry
		completed++
		log.Printf("fixture %03d/%03d index=%03d bytes=%d tokens=%d gzip/raw=%.3f", completed, *count, result.index, result.entry.Bytes, result.entry.Tokens, result.entry.GzipToRawRatio)
		if completed%32 == 0 {
			runtime.GC()
		}
	}
	if firstErr != nil {
		return firstErr
	}
	if completed != *count {
		return fmt.Errorf("fixture generation stopped early: completed=%d want=%d", completed, *count)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*dir, "manifest.json"), append(raw, '\n'), 0o600)
}

func generateFixtureEntry(encoding *tiktoken.Tiktoken, dir, model, encodingName string, index, target int, tolerance float64) (fixtureEntry, error) {
	key := fmt.Sprintf("extreme-%03d", index)
	body, tokens, err := generateFixture(encoding, model, key, target, tolerance)
	if err != nil {
		return fixtureEntry{}, err
	}
	name := fmt.Sprintf("fixture-%03d.json", index)
	if err = os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
		return fixtureEntry{}, err
	}
	sum := sha256.Sum256(body)
	ratio, err := gzipRatio(body)
	if err != nil {
		return fixtureEntry{}, err
	}
	if ratio < 0.70 {
		return fixtureEntry{}, fmt.Errorf("fixture is too compressible: gzip/raw %.4f", ratio)
	}
	return fixtureEntry{
		File: name, Bytes: int64(len(body)), Tokens: tokens, SHA256: hex.EncodeToString(sum[:]), GzipToRawRatio: ratio,
		PromptCacheKey: key, VerificationKind: "tiktoken:" + encodingName,
	}, nil
}

func generateFixture(encoding *tiktoken.Tiktoken, model, key string, target int, tolerance float64) ([]byte, int, error) {
	chars := max(target*3/2, 1024)
	low, high := float64(target)*(1-tolerance), float64(target)*(1+tolerance)
	var body []byte
	var tokens int
	for attempt := 0; attempt < 10; attempt++ {
		payload, err := randomURLText(chars)
		if err != nil {
			return nil, 0, err
		}
		body, err = json.Marshal(struct {
			Model          string `json:"model"`
			Stream         bool   `json:"stream"`
			PromptCacheKey string `json:"prompt_cache_key"`
			Input          string `json:"input"`
		}{Model: model, Stream: true, PromptCacheKey: key, Input: payload})
		if err != nil {
			return nil, 0, err
		}
		tokens = len(encoding.EncodeOrdinary(string(body)))
		if float64(tokens) >= low && float64(tokens) <= high {
			return body, tokens, nil
		}
		if tokens <= 0 {
			return nil, 0, errors.New("tokenizer returned no tokens")
		}
		next := int(math.Round(float64(chars) * float64(target) / float64(tokens)))
		if next == chars {
			if tokens < target {
				next += max(64, chars/1000)
			} else {
				next -= max(64, chars/1000)
			}
		}
		chars = max(next, 1)
	}
	return nil, 0, fmt.Errorf("failed to converge: tokens=%d target=%d tolerance=%.4f", tokens, target, tolerance)
}

func randomURLText(length int) (string, error) {
	raw := make([]byte, (length*3+3)/4)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) < length {
		return "", errors.New("random encoder produced a short payload")
	}
	return encoded[:length], nil
}

func gzipRatio(raw []byte) (float64, error) {
	if len(raw) == 0 {
		return 1, nil
	}
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if err != nil {
		return 0, err
	}
	if _, err = writer.Write(raw); err != nil {
		return 0, err
	}
	if err = writer.Close(); err != nil {
		return 0, err
	}
	return float64(compressed.Len()) / float64(len(raw)), nil
}

func ensureEmptyDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("fixture directory must be empty: %s", dir)
	}
	return nil
}

func verifyCommand(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := flags.String("dir", "", "fixture directory")
	minimum := flags.Int("minimum", 256, "minimum fixture count")
	workers := flags.Int("workers", runtime.NumCPU(), "parallel fixture verifiers")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *workers < 1 {
		return errors.New("positive workers are required")
	}
	manifest, _, minTokens, maxTokens, err := verifyFixtures(*dir, *minimum, false, *workers)
	if err != nil {
		return err
	}
	log.Printf("verified %d fixtures model=%s encoding=%s token_range=%d..%d", len(manifest.Fixtures), manifest.Model, manifest.Encoding, minTokens, maxTokens)
	return nil
}

func verifyFixtures(dir string, minimum int, loadBodies bool, workers int) (fixtureManifest, [][]byte, int, int, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fixtureManifest{}, nil, 0, 0, err
	}
	var manifest fixtureManifest
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return manifest, nil, 0, 0, err
	}
	if manifest.Version != fixtureManifestVersion || len(manifest.Fixtures) == 0 || len(manifest.Fixtures) < minimum || manifest.TargetTokens <= 0 || manifest.Tolerance <= 0 || manifest.Tolerance > 0.1 || strings.TrimSpace(manifest.Encoding) == "" {
		return manifest, nil, 0, 0, errors.New("invalid or undersized fixture manifest")
	}
	if expected := tokenizerChecksum(manifest.Encoding); expected != "" && !strings.EqualFold(manifest.EncodingSHA, expected) {
		return manifest, nil, 0, 0, fmt.Errorf("fixture tokenizer checksum mismatch: got %q want %q", manifest.EncodingSHA, expected)
	}
	encoding, err := tiktoken.GetEncoding(manifest.Encoding)
	if err != nil {
		return manifest, nil, 0, 0, err
	}
	var bodies [][]byte
	if loadBodies {
		bodies = make([][]byte, len(manifest.Fixtures))
	}
	minTokens, maxTokens := math.MaxInt, 0
	lower := float64(manifest.TargetTokens) * (1 - manifest.Tolerance)
	upper := float64(manifest.TargetTokens) * (1 + manifest.Tolerance)
	type verifiedFixture struct {
		index  int
		body   []byte
		tokens int
		err    error
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workerCount := min(max(workers, 1), len(manifest.Fixtures))
	jobs := make(chan int)
	results := make(chan verifiedFixture, workerCount)
	var group sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			defer supervisor.Recover("extreme-load-fixture-verifier")
			for {
				select {
				case <-ctx.Done():
					return
				case index, ok := <-jobs:
					if !ok {
						return
					}
					body, tokens, verifyErr := verifyFixtureEntry(dir, manifest, encoding, index, lower, upper)
					select {
					case results <- verifiedFixture{index: index, body: body, tokens: tokens, err: verifyErr}:
					case <-ctx.Done():
						return
					}
					if verifyErr != nil {
						return
					}
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		defer supervisor.Recover("extreme-load-verify-jobs")
		for index := range manifest.Fixtures {
			select {
			case jobs <- index:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		defer close(results)
		defer supervisor.Recover("extreme-load-verify-results")
		group.Wait()
	}()
	verified := 0
	var firstErr error
	for result := range results {
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
				cancel()
			}
			continue
		}
		verified++
		minTokens, maxTokens = min(minTokens, result.tokens), max(maxTokens, result.tokens)
		if loadBodies {
			bodies[result.index] = result.body
		}
		if verified%32 == 0 {
			runtime.GC()
		}
	}
	if firstErr != nil {
		return manifest, nil, 0, 0, firstErr
	}
	if verified != len(manifest.Fixtures) {
		return manifest, nil, 0, 0, fmt.Errorf("fixture verification stopped early: verified=%d want=%d", verified, len(manifest.Fixtures))
	}
	return manifest, bodies, minTokens, maxTokens, nil
}

func verifyFixtureEntry(dir string, manifest fixtureManifest, encoding *tiktoken.Tiktoken, index int, lower, upper float64) ([]byte, int, error) {
	fixture := manifest.Fixtures[index]
	expectedFile := fmt.Sprintf("fixture-%03d.json", index)
	expectedKey := fmt.Sprintf("extreme-%03d", index)
	if fixture.File != expectedFile || filepath.Base(fixture.File) != fixture.File || fixture.PromptCacheKey != expectedKey || fixture.VerificationKind != "tiktoken:"+manifest.Encoding {
		return nil, 0, fmt.Errorf("fixture %d identity metadata is invalid", index)
	}
	body, err := os.ReadFile(filepath.Join(dir, fixture.File))
	if err != nil {
		return nil, 0, err
	}
	sum := sha256.Sum256(body)
	tokens := len(encoding.EncodeOrdinary(string(body)))
	ratio, err := gzipRatio(body)
	if err != nil {
		return nil, 0, err
	}
	if int64(len(body)) != fixture.Bytes || hex.EncodeToString(sum[:]) != fixture.SHA256 || tokens != fixture.Tokens || float64(tokens) < lower || float64(tokens) > upper || ratio < 0.70 || math.Abs(ratio-fixture.GzipToRawRatio) > 1e-12 || !json.Valid(body) {
		return nil, 0, fmt.Errorf("fixture %d verification failed: bytes=%d tokens=%d ratio=%.4f", index, len(body), tokens, ratio)
	}
	return body, tokens, nil
}

func tokenizerChecksum(encoding string) string {
	if encoding == "o200k_base" {
		return o200kBaseSHA256
	}
	return ""
}

func mockCommand(args []string) error {
	flags := flag.NewFlagSet("mock", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:19443", "TLS listen address")
	certFile := flags.String("cert", "", "TLS certificate")
	keyFile := flags.String("key", "", "TLS private key")
	inputTokens := flags.Int("input-tokens", 1_000_000, "usage input token count")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *certFile == "" || *keyFile == "" {
		return errors.New("cert and key are required")
	}
	var total, h2Requests, bytesRead atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]int64{"requests": total.Load(), "http2_requests": h2Requests.Load(), "bytes_read": bytesRead.Load()})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		requestNumber := total.Add(1)
		if r.ProtoMajor != 2 {
			http.Error(w, "HTTP/2 required", http.StatusHTTPVersionNotSupported)
			return
		}
		h2Requests.Add(1)
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bytesRead.Add(n)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_load_%d\",\"status\":\"completed\",\"model\":\"gpt-5.6-sol\",\"output\":[],\"usage\":{\"input_tokens\":%d,\"output_tokens\":1,\"total_tokens\":%d}}}\n\ndata: [DONE]\n\n", requestNumber, *inputTokens, *inputTokens+1)
	})
	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}}
	if err := http2.ConfigureServer(server, &http2.Server{}); err != nil {
		return err
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		defer supervisor.Recover("extreme-load-mock-shutdown")
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	log.Printf("protocol-real mock listening https://%s (TLS + HTTP/2)", *listen)
	err := server.ListenAndServeTLS(*certFile, *keyFile)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func seedCommand(args []string) error {
	flags := flag.NewFlagSet("seed", flag.ContinueOnError)
	database := flags.String("database", "", "SQLite database path")
	accounts := flags.Int("accounts", 64, "number of active Codex API-key accounts")
	group := flags.String("group", "cyber", "account group")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *database == "" || *accounts < 1 {
		return errors.New("database and positive accounts are required")
	}
	store, err := storage.Open(*database)
	if err != nil {
		return err
	}
	defer store.Close()
	if err = store.Init(context.Background()); err != nil {
		return err
	}
	for index := 0; index < *accounts; index++ {
		id := fmt.Sprintf("load-%03d", index)
		if err = store.UpsertAccount(context.Background(), storage.Account{ID: id, Label: id, GroupName: *group, Provider: "codex", Status: "active"}, storage.AccountToken{OpenAIAPIKey: "sk-load-" + id}); err != nil {
			return err
		}
	}
	log.Printf("seeded %d accounts in %s", *accounts, *database)
	return nil
}

func runCommand(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	dir := flags.String("dir", "", "verified fixture directory")
	endpoint := flags.String("endpoint", "http://127.0.0.1:18787/v1/responses", "gateway endpoint")
	rps := flags.Int("rps", 100, "target requests per second")
	duration := flags.Duration("duration", 10*time.Second, "timed load duration")
	concurrency := flags.Int("concurrency", 256, "load worker count")
	minimum := flags.Int("minimum", 256, "minimum fixture count")
	fixtureWorkers := flags.Int("fixture-workers", runtime.NumCPU(), "parallel pre-timing fixture verifiers")
	timeout := flags.Duration("timeout", 600*time.Second, "per-request timeout")
	hard := flags.Bool("hard", false, "fail unless all requests succeed and achieved RPS meets the strict minimum")
	minimumAchievedRPS := flags.Float64("minimum-achieved-rps", 0, "hard-mode minimum completed RPS; defaults to target RPS")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *rps < 1 || *duration <= 0 || *concurrency < 1 || *fixtureWorkers < 1 || *minimumAchievedRPS < 0 {
		return errors.New("dir, positive rps/duration/concurrency/fixture-workers and non-negative minimum-achieved-rps are required")
	}
	requiredRPS := *minimumAchievedRPS
	if requiredRPS == 0 {
		requiredRPS = float64(*rps)
	}
	manifest, bodies, minTokens, maxTokens, err := verifyFixtures(*dir, *minimum, true, *fixtureWorkers)
	if err != nil {
		return fmt.Errorf("pre-timing fixture verification: %w", err)
	}
	requests := max(1, int(math.Round(duration.Seconds()*float64(*rps))))
	client := &http.Client{Transport: &http.Transport{
		MaxIdleConns: 512, MaxIdleConnsPerHost: 256, MaxConnsPerHost: 512, IdleConnTimeout: 90 * time.Second,
		DialContext: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true,
	}, Timeout: *timeout}
	type response struct {
		latency time.Duration
		bytes   int64
		err     error
	}
	jobs := make(chan int, *concurrency)
	results := make(chan response, requests)
	var workers sync.WaitGroup
	for worker := 0; worker < *concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer supervisor.Recover("extreme-load-worker")
			defer workers.Done()
			for index := range jobs {
				body := bodies[index%len(bodies)]
				started := time.Now()
				req, requestErr := http.NewRequest(http.MethodPost, *endpoint, bytes.NewReader(body))
				if requestErr == nil {
					req.Header.Set("Content-Type", "application/json")
					var resp *http.Response
					resp, requestErr = client.Do(req)
					if requestErr == nil {
						_, requestErr = io.Copy(io.Discard, resp.Body)
						closeErr := resp.Body.Close()
						if requestErr == nil {
							requestErr = closeErr
						}
						if requestErr == nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
							requestErr = fmt.Errorf("HTTP %d", resp.StatusCode)
						}
					}
				}
				results <- response{latency: time.Since(started), bytes: int64(len(body)), err: requestErr}
			}
		}()
	}
	started := time.Now()
	interval := time.Second / time.Duration(*rps)
	for index := 0; index < requests; index++ {
		due := started.Add(time.Duration(index) * interval)
		if wait := time.Until(due); wait > 0 {
			timer := time.NewTimer(wait)
			<-timer.C
		}
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	close(results)
	elapsed := time.Since(started)
	latencies := make([]time.Duration, 0, requests)
	var succeeded, failed int
	var sent int64
	for result := range results {
		latencies = append(latencies, result.latency)
		sent += result.bytes
		if result.err != nil {
			failed++
			if failed <= 10 {
				log.Printf("request failed: %v", result.err)
			}
		} else {
			succeeded++
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	result := loadResult{
		Requests: requests, Succeeded: succeeded, Failed: failed, TargetRPS: *rps, MinimumAchievedRPS: requiredRPS, AchievedRPS: float64(requests) / elapsed.Seconds(), ElapsedSeconds: elapsed.Seconds(),
		P50Millis: percentileMillis(latencies, 0.50), P95Millis: percentileMillis(latencies, 0.95), P99Millis: percentileMillis(latencies, 0.99), BytesSent: sent,
		VerifiedFixtures: len(manifest.Fixtures), VerifiedMinTokens: minTokens, VerifiedMaxTokens: maxTokens,
	}
	raw, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(raw))
	if *hard && (failed != 0 || result.AchievedRPS < requiredRPS) {
		return fmt.Errorf("hard threshold failed: failures=%d achieved_rps=%.2f minimum_achieved_rps=%.2f target=%d", failed, result.AchievedRPS, requiredRPS, *rps)
	}
	return nil
}

func percentileMillis(values []time.Duration, percentile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	index := int(math.Ceil(float64(len(values))*percentile)) - 1
	index = min(max(index, 0), len(values)-1)
	return float64(values[index]) / float64(time.Millisecond)
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
