// Package tokenizer produces exact GPT token counts using an o200k_base vocabulary
// embedded in the binary.
//
// It exists because the pool previously answered every token question with
// virtual.EstimateTokensJSON — utf8.RuneCount(rawJSON)/4 — which is wrong in two
// compounding directions: it counts JSON structure (field names, braces, quotes,
// \uXXXX escapes) as if it were content, and it divides runes by four regardless of
// script. o200k spends roughly one token per CJK character, so runes/4 undercounts
// Chinese text by three to four times while simultaneously overcounting the envelope
// of a tool-heavy request. On a long Chinese conversation the two errors do not cancel
// and the reported total drifts from the provider's own accounting by tens of thousands
// of tokens.
//
// The vocabulary is embedded, gzipped, rather than fetched: tiktoken-go's default
// loader downloads it from openaipublic.blob.core.windows.net on first use, which would
// add both a runtime failure mode on restricted networks and an outbound connection
// from the relay host that has nothing to do with serving a request.
//
// Scope: o200k_base is OpenAI's tokenizer. It is NOT Anthropic's, so this package is
// only consulted for GPT-family models; Claude token questions are answered by the
// provider's own count_tokens endpoint.
package tokenizer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	_ "embed"

	"github.com/pkoukk/tiktoken-go"
)

//go:embed o200k_base.tiktoken.gz
var o200kBaseGz []byte

// o200kBaseURL is the blob path tiktoken-go asks its loader for when building the
// o200k_base encoding. The offline loader matches on it so that a request for any
// OTHER encoding fails loudly instead of silently reaching the network.
const o200kBaseURL = "https://openaipublic.blob.core.windows.net/encodings/o200k_base.tiktoken"

type embeddedBpeLoader struct{}

func (embeddedBpeLoader) LoadTiktokenBpe(tiktokenBpeFile string) (map[string]int, error) {
	if tiktokenBpeFile != o200kBaseURL {
		return nil, fmt.Errorf("tokenizer: only o200k_base is embedded, refusing network fetch for %q", tiktokenBpeFile)
	}
	return parseEmbeddedRanks()
}

func parseEmbeddedRanks() (map[string]int, error) {
	reader, err := gzip.NewReader(bytes.NewReader(o200kBaseGz))
	if err != nil {
		return nil, fmt.Errorf("tokenizer: open embedded vocabulary: %w", err)
	}
	defer reader.Close()
	ranks := make(map[string]int, 200_000)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		token, rankText, ok := strings.Cut(line, " ")
		if !ok {
			return nil, errors.New("tokenizer: malformed vocabulary line")
		}
		decoded, err := base64.StdEncoding.DecodeString(token)
		if err != nil {
			return nil, fmt.Errorf("tokenizer: decode vocabulary token: %w", err)
		}
		rank, err := strconv.Atoi(rankText)
		if err != nil {
			return nil, fmt.Errorf("tokenizer: parse vocabulary rank: %w", err)
		}
		ranks[string(decoded)] = rank
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("tokenizer: read embedded vocabulary: %w", err)
	}
	if len(ranks) == 0 {
		return nil, errors.New("tokenizer: embedded vocabulary is empty")
	}
	return ranks, nil
}

var (
	once     sync.Once
	encoding *tiktoken.Tiktoken
	initErr  error
)

// encoder builds the o200k_base encoder once. tiktoken.SetBpeLoader is process-global,
// so it is installed here before the first encoding is constructed; every caller in
// this process then resolves through the embedded vocabulary.
func encoder() (*tiktoken.Tiktoken, error) {
	once.Do(func() {
		tiktoken.SetBpeLoader(embeddedBpeLoader{})
		encoding, initErr = tiktoken.GetEncoding(tiktoken.MODEL_O200K_BASE)
	})
	return encoding, initErr
}

// Available reports whether exact counting is usable. Callers fall back to their
// previous estimate when it is not, so a vocabulary problem degrades the number
// instead of failing the request.
func Available() bool {
	_, err := encoder()
	return err == nil
}

// CountText returns the exact o200k token count for s. ok is false when the encoder
// could not be built, in which case the caller must keep its own estimate.
func CountText(s string) (int64, bool) {
	if s == "" {
		return 0, true
	}
	enc, err := encoder()
	if err != nil {
		return 0, false
	}
	return int64(len(enc.Encode(s, nil, nil))), true
}
