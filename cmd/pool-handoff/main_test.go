package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func unixServer(t *testing.T, socket, response string, entered, release chan struct{}) *http.Server {
	t.Helper()
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if entered != nil {
			close(entered)
			<-release
		}
		_, _ = io.WriteString(w, response)
	})}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close(); os.Remove(socket) })
	return s
}

func replaceLink(t *testing.T, link, target string) {
	t.Helper()
	tmp := link + ".next"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicSwitchKeepsOldRequestAndRoutesNewRequest(t *testing.T) {
	dir := t.TempDir()
	oldSocket, newSocket := filepath.Join(dir, "old.sock"), filepath.Join(dir, "new.sock")
	link := filepath.Join(dir, "active.sock")
	entered, release := make(chan struct{}), make(chan struct{})
	unixServer(t, oldSocket, "old", entered, release)
	unixServer(t, newSocket, "new", nil, nil)
	replaceLink(t, link, oldSocket)

	front := httptest.NewServer(newHandoff(link))
	defer front.Close()
	oldResult := make(chan string, 1)
	go func() {
		resp, _ := http.Get(front.URL + "/stream")
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		oldResult <- string(body)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("old request did not reach worker")
	}
	replaceLink(t, link, newSocket)
	resp, err := http.Get(front.URL + "/next")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "new" {
		t.Fatalf("new request body = %q", body)
	}
	close(release)
	if got := <-oldResult; got != "old" {
		t.Fatalf("old request body = %q", got)
	}
}

func TestAtomicSwitchDoesNotCutSSE(t *testing.T) {
	dir := t.TempDir()
	oldSocket, newSocket := filepath.Join(dir, "old-sse.sock"), filepath.Join(dir, "new-sse.sock")
	link := filepath.Join(dir, "active-sse.sock")
	ln, err := net.Listen("unix", oldSocket)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	old := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: old-start\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: old-end\n\n")
	})}
	go old.Serve(ln)
	t.Cleanup(func() { old.Close() })
	unixServer(t, newSocket, "new", nil, nil)
	replaceLink(t, link, oldSocket)
	front := httptest.NewServer(newHandoff(link))
	defer front.Close()

	resp, err := http.Get(front.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: old-start\n" {
		t.Fatalf("first SSE event = %q, %v", line, err)
	}
	pauseResp := postControl(t, front.URL, pausePath)
	_ = pauseResp.Body.Close()
	replaceLink(t, link, newSocket)
	close(release)
	rest, err := io.ReadAll(reader)
	_ = resp.Body.Close()
	if err != nil || !strings.Contains(string(rest), "data: old-end") {
		t.Fatalf("old SSE tail = %q, %v", rest, err)
	}

	nextResult := make(chan string, 1)
	go func() {
		next, getErr := http.Get(front.URL + "/next")
		if getErr != nil {
			nextResult <- "error: " + getErr.Error()
			return
		}
		nextBody, _ := io.ReadAll(next.Body)
		_ = next.Body.Close()
		nextResult <- string(nextBody)
	}()
	waitQueued(t, front.Config.Handler.(*handoff), 1)
	resumeResp := postControl(t, front.URL, resumePath)
	_ = resumeResp.Body.Close()
	if got := <-nextResult; got != "new" {
		t.Fatalf("new request body = %q", got)
	}
}

func TestAtomicSwitchDoesNotCutWebSocket(t *testing.T) {
	dir := t.TempDir()
	oldSocket, newSocket := filepath.Join(dir, "old-ws.sock"), filepath.Join(dir, "new-ws.sock")
	link := filepath.Join(dir, "active-ws.sock")
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	listen := func(socket, label string, release <-chan struct{}) *http.Server {
		ln, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			if err := conn.WriteMessage(websocket.TextMessage, []byte(label+"-start")); err != nil {
				return
			}
			if release != nil {
				<-release
				_ = conn.WriteMessage(websocket.TextMessage, []byte(label+"-end"))
			}
		})}
		go srv.Serve(ln)
		t.Cleanup(func() {
			_ = srv.Close()
			_ = os.Remove(socket)
		})
		return srv
	}

	release := make(chan struct{})
	listen(oldSocket, "old", release)
	listen(newSocket, "new", nil)
	replaceLink(t, link, oldSocket)
	h := newHandoff(link)
	front := httptest.NewServer(h)
	defer front.Close()
	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/responses"

	oldConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer oldConn.Close()
	_, first, err := oldConn.ReadMessage()
	if err != nil || string(first) != "old-start" {
		t.Fatalf("old WebSocket first message = %q, %v", first, err)
	}

	pauseResp := postControl(t, front.URL, pausePath)
	_ = pauseResp.Body.Close()
	replaceLink(t, link, newSocket)
	close(release)
	_, tail, err := oldConn.ReadMessage()
	if err != nil || string(tail) != "old-end" {
		t.Fatalf("old WebSocket tail = %q, %v", tail, err)
	}

	type wsResult struct {
		message string
		err     error
	}
	newResult := make(chan wsResult, 1)
	go func() {
		newConn, _, dialErr := websocket.DefaultDialer.Dial(wsURL, nil)
		if dialErr != nil {
			newResult <- wsResult{err: dialErr}
			return
		}
		defer newConn.Close()
		_, current, readErr := newConn.ReadMessage()
		newResult <- wsResult{message: string(current), err: readErr}
	}()
	waitQueued(t, h, 1)
	resumeResp := postControl(t, front.URL, resumePath)
	_ = resumeResp.Body.Close()
	current := <-newResult
	if current.err != nil || current.message != "new-start" {
		t.Fatalf("new WebSocket message = %q, %v", current.message, current.err)
	}
}

func TestConcurrentSwitchHasNoFailedUnaryRequests(t *testing.T) {
	dir := t.TempDir()
	oldSocket, newSocket := filepath.Join(dir, "old-load.sock"), filepath.Join(dir, "new-load.sock")
	link := filepath.Join(dir, "active-load.sock")
	unixServer(t, oldSocket, "old", nil, nil)
	unixServer(t, newSocket, "new", nil, nil)
	replaceLink(t, link, oldSocket)
	front := httptest.NewServer(newHandoff(link))
	defer front.Close()

	const requests = 200
	start := make(chan struct{})
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Get(front.URL + "/unary")
			if err == nil {
				_, err = io.Copy(io.Discard, resp.Body)
				err = errors.Join(err, resp.Body.Close())
				if resp.StatusCode != http.StatusOK {
					err = errors.Join(err, fmt.Errorf("status %d", resp.StatusCode))
				}
			}
			errs <- err
		}()
	}
	close(start)
	replaceLink(t, link, newSocket)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func waitQueued(t *testing.T, h *handoff, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, queued, _, _, _ := h.gate.snapshot()
		if queued == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	_, queued, _, _, _ := h.gate.snapshot()
	t.Fatalf("queued = %d, want %d", queued, want)
}

func postControl(t *testing.T, baseURL, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("POST %s status = %d: %s", path, resp.StatusCode, body)
	}
	return resp
}

func TestAdmissionPauseQueuesUntilAtomicSwitchAndResume(t *testing.T) {
	dir := t.TempDir()
	oldSocket, newSocket := filepath.Join(dir, "old-pause.sock"), filepath.Join(dir, "new-pause.sock")
	link := filepath.Join(dir, "active-pause.sock")
	oldEntered := make(chan struct{})
	oldRelease := make(chan struct{})
	unixServer(t, oldSocket, "old", oldEntered, oldRelease)
	unixServer(t, newSocket, "new", nil, nil)
	replaceLink(t, link, oldSocket)

	stateFile := filepath.Join(dir, "admission-paused.json")
	h := newHandoffWithState(link, stateFile, "test-handoff")
	front := httptest.NewServer(h)
	defer front.Close()

	resp := postControl(t, front.URL, pausePath+"?reason=install&release=r2")
	_ = resp.Body.Close()
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("pause state was not persisted: %v", err)
	}

	type result struct {
		body string
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		resp, err := http.Get(front.URL + "/v1/responses")
		if err != nil {
			resultCh <- result{err: err}
			return
		}
		body, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		resultCh <- result{body: string(body), err: errors.Join(readErr, closeErr)}
	}()
	waitQueued(t, h, 1)
	select {
	case <-oldEntered:
		t.Fatal("paused request reached the old worker")
	default:
	}

	replaceLink(t, link, newSocket)
	resp = postControl(t, front.URL, resumePath)
	_ = resp.Body.Close()
	got := <-resultCh
	if got.err != nil || got.body != "new" {
		t.Fatalf("resumed request = body %q, err %v", got.body, got.err)
	}
	if _, err := os.Stat(stateFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pause state still exists after resume: %v", err)
	}
	close(oldRelease)
}

func TestAdmissionPauseReleasesAllConcurrentRequests(t *testing.T) {
	dir := t.TempDir()
	socket, link := filepath.Join(dir, "worker-many.sock"), filepath.Join(dir, "active-many.sock")
	unixServer(t, socket, "new", nil, nil)
	replaceLink(t, link, socket)
	h := newHandoff(link)
	front := httptest.NewServer(h)
	defer front.Close()
	resp := postControl(t, front.URL, pausePath)
	_ = resp.Body.Close()

	const requests = 64
	errs := make(chan error, requests)
	for i := 0; i < requests; i++ {
		go func() {
			resp, err := http.Get(front.URL + "/queued")
			if err == nil {
				body, readErr := io.ReadAll(resp.Body)
				err = errors.Join(readErr, resp.Body.Close())
				if string(body) != "new" {
					err = errors.Join(err, fmt.Errorf("body %q", body))
				}
			}
			errs <- err
		}()
	}
	waitQueued(t, h, requests)
	resp = postControl(t, front.URL, resumePath)
	_ = resp.Body.Close()
	for i := 0; i < requests; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	waitQueued(t, h, 0)
}

func TestAdmissionControlRejectsNonLoopbackAndWrongMethod(t *testing.T) {
	h := newHandoff(filepath.Join(t.TempDir(), "missing.sock"))

	req := httptest.NewRequest(http.MethodPost, pausePath, nil)
	req.RemoteAddr = "198.51.100.10:1234"
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("remote pause status = %d", recorder.Code)
	}
	paused, _, _, _, _ := h.gate.snapshot()
	if paused {
		t.Fatal("remote request paused admission")
	}

	req = httptest.NewRequest(http.MethodGet, pausePath, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET pause status = %d", recorder.Code)
	}
}

func TestBackendFailureUsesFixedPublicErrorFirewall(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "secret-account-worker.sock")
	h := newHandoff(missing)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5"}`))
	recorder := httptest.NewRecorder()

	h.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	requestID := recorder.Header().Get("X-Request-ID")
	if !regexp.MustCompile(`^REQ-[A-F0-9]{16}$`).MatchString(requestID) {
		t.Fatalf("request id = %q", requestID)
	}
	if recorder.Header().Get("Retry-After") != publicRetryAfter {
		t.Fatalf("Retry-After = %q", recorder.Header().Get("Retry-After"))
	}
	for name := range recorder.Header() {
		switch http.CanonicalHeaderKey(name) {
		case "Content-Type", "Retry-After", "X-Request-Id":
		default:
			t.Fatalf("unexpected public response header %q", name)
		}
	}
	body := recorder.Body.String()
	for _, forbidden := range []string{missing, "secret-account", "dial unix", "worker_unavailable"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("public body leaked %q: %s", forbidden, body)
		}
	}
	for _, required := range []string{
		`"type":"server_error"`,
		`"code":"service_unavailable"`,
		publicServiceUnavailableText,
		`"request_id":"` + requestID + `"`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("public body missing %q: %s", required, body)
		}
	}
}

func TestPublicHandoffStatusDoesNotExposeBackendPathOrRawError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "secret-backend.sock")
	h := newHandoff(missing)
	req := httptest.NewRequest(http.MethodGet, "/handoffz", nil)
	recorder := httptest.NewRecorder()

	h.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	body := recorder.Body.String()
	for _, forbidden := range []string{missing, "secret-backend", "no such file", "active_backend"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("public handoff status leaked %q: %s", forbidden, body)
		}
	}
	if !strings.Contains(body, `"code":"worker_unavailable"`) {
		t.Fatalf("public handoff status missing safe code: %s", body)
	}

	trusted := httptest.NewRecorder()
	h.serveStatus(trusted, true)
	if !strings.Contains(trusted.Body.String(), missing) {
		t.Fatalf("trusted local status must retain backend diagnostics: %s", trusted.Body.String())
	}
}

func TestCanceledQueuedRequestDoesNotLeakQueueCount(t *testing.T) {
	h := newHandoff(filepath.Join(t.TempDir(), "missing.sock"))
	if err := h.gate.pause("test", ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(recorder, req)
		close(done)
	}()
	waitQueued(t, h, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled request stayed queued")
	}
	waitQueued(t, h, 0)
}

func TestPauseStateSurvivesHandoffRestart(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "admission-paused.json")
	first := newHandoffWithState(filepath.Join(dir, "active.sock"), stateFile, "first")
	if err := first.gate.pause("install", "release-2"); err != nil {
		t.Fatal(err)
	}
	restarted := newHandoffWithState(filepath.Join(dir, "active.sock"), stateFile, "restarted")
	paused, _, _, reason, release := restarted.gate.snapshot()
	if !paused || reason != "install" || release != "release-2" {
		t.Fatalf("restarted pause = paused %v, reason %q, release %q", paused, reason, release)
	}
	if err := restarted.gate.resume(); err != nil {
		t.Fatal(err)
	}
}

// This is the process-level shape of the one-time systemd migration: the activation
// socket owns the original TCP listener, the legacy service and the independently named
// handoff temporarily hold duplicate descriptors, and stopping the legacy service closes
// only its accept loop while its established stream continues to completion.
func TestSharedActivationSocketLetsLegacyDrainWithoutBlockingHandoff(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp := base.(*net.TCPListener)
	oldFile, err := tcp.File()
	if err != nil {
		t.Fatal(err)
	}
	handoffFile, err := tcp.File()
	if err != nil {
		t.Fatal(err)
	}
	_ = base.Close()
	oldListener, err := net.FileListener(oldFile)
	_ = oldFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	handoffListener, err := net.FileListener(handoffFile)
	_ = handoffFile.Close()
	if err != nil {
		t.Fatal(err)
	}

	oldEntered, oldRelease := make(chan struct{}), make(chan struct{})
	oldServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/legacy-stream" {
			_, _ = io.WriteString(w, "legacy")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: legacy-start\n\n")
		w.(http.Flusher).Flush()
		close(oldEntered)
		<-oldRelease
		_, _ = io.WriteString(w, "data: legacy-end\n\n")
	})}
	oldServeDone := make(chan error, 1)
	go func() { oldServeDone <- oldServer.Serve(oldListener) }()

	publicURL := "http://" + tcp.Addr().String()
	stream, err := http.Get(publicURL + "/legacy-stream")
	if err != nil {
		t.Fatal(err)
	}
	streamReader := bufio.NewReader(stream.Body)
	line, err := streamReader.ReadString('\n')
	if err != nil || line != "data: legacy-start\n" {
		t.Fatalf("legacy stream first event = %q, %v", line, err)
	}
	<-oldEntered

	dir := t.TempDir()
	workerSocket, activeLink := filepath.Join(dir, "new-worker.sock"), filepath.Join(dir, "active.sock")
	unixServer(t, workerSocket, "new", nil, nil)
	replaceLink(t, activeLink, workerSocket)
	h := newHandoff(activeLink)
	if err := h.gate.pause("migration", "release-2"); err != nil {
		t.Fatal(err)
	}
	handoffServer := &http.Server{Handler: h}
	handoffServeDone := make(chan error, 1)
	go func() { handoffServeDone <- handoffServer.Serve(handoffListener) }()

	legacyShutdownDone := make(chan error, 1)
	go func() { legacyShutdownDone <- oldServer.Shutdown(context.Background()) }()
	select {
	case <-oldServeDone: // its accept descriptor closed; the stream handler still runs
	case <-time.After(2 * time.Second):
		t.Fatal("legacy accept loop did not stop promptly")
	}

	newResult := make(chan string, 1)
	go func() {
		resp, getErr := http.Get(publicURL + "/after-migration")
		if getErr != nil {
			newResult <- "error: " + getErr.Error()
			return
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		newResult <- string(body)
	}()
	waitQueued(t, h, 1)

	close(oldRelease)
	legacyTail, err := io.ReadAll(streamReader)
	_ = stream.Body.Close()
	if err != nil || !strings.Contains(string(legacyTail), "data: legacy-end") {
		t.Fatalf("legacy stream tail = %q, %v", legacyTail, err)
	}
	select {
	case err := <-legacyShutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("legacy service did not finish after its stream drained")
	}

	if err := h.gate.resume(); err != nil {
		t.Fatal(err)
	}
	if got := <-newResult; got != "new" {
		t.Fatalf("queued request after migration = %q", got)
	}
	_ = handoffServer.Close()
	<-handoffServeDone
}
