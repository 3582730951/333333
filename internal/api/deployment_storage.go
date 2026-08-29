package api

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codex-account-pool/internal/sysmetrics"
)

const deploymentStorageCacheTTL = 15 * time.Second

type deploymentReaperStatus struct {
	ReleaseID         string `json:"release_id"`
	PID               int    `json:"pid,omitempty"`
	Bytes             int64  `json:"bytes"`
	AgeSeconds        int64  `json:"age_seconds,omitempty"`
	CriticalInflight  int64  `json:"critical_inflight"`
	ResumableInflight int64  `json:"resumable_inflight"`
	State             string `json:"state"`
	HeartbeatAt       int64  `json:"heartbeat_at,omitempty"`
	LastError         string `json:"last_error,omitempty"`
}

type deploymentStorageStatus struct {
	CurrentRelease         string                   `json:"current_release"`
	TotalReleaseBytes      int64                    `json:"total_release_bytes"`
	ReleaseBudgetBytes     int64                    `json:"release_budget_bytes"`
	FreeBytes              int64                    `json:"free_bytes"`
	FreeReserveBytes       int64                    `json:"free_reserve_bytes"`
	PredictedPeakBytes     int64                    `json:"predicted_peak_bytes"`
	BackupBytes            int64                    `json:"backup_bytes"`
	ConsoleGenerationBytes int64                    `json:"console_generation_bytes"`
	AdmissionPauseMillis   int64                    `json:"admission_pause_duration_ms"`
	Draining               []deploymentReaperStatus `json:"draining"`
	ReaperHeartbeatAt      int64                    `json:"reaper_heartbeat_at,omitempty"`
	LastReclaimError       string                   `json:"last_reclaim_error,omitempty"`
	UpdatedAt              int64                    `json:"updated_at"`
}

func (s *Server) deploymentStorageStatus() deploymentStorageStatus {
	if s == nil {
		return deploymentStorageStatus{}
	}
	now := time.Now()
	s.deploymentStorageMu.Lock()
	defer s.deploymentStorageMu.Unlock()
	if !s.deploymentStorageCachedAt.IsZero() && now.Sub(s.deploymentStorageCachedAt) < deploymentStorageCacheTTL {
		return s.deploymentStorageCached
	}
	status := s.readDeploymentStorageStatus(now)
	s.deploymentStorageCached = status
	s.deploymentStorageCachedAt = now
	return status
}

func (s *Server) readDeploymentStorageStatus(now time.Time) deploymentStorageStatus {
	status := deploymentStorageStatus{CurrentRelease: strings.TrimSpace(s.releaseID), UpdatedAt: now.Unix()}
	installDataDir := deploymentInstallDataDir(s.cfg.DataDir)
	statePath := filepath.Join(installDataDir, "run", "deployment-storage.json")
	if raw, err := os.ReadFile(statePath); err == nil && len(raw) <= 128<<10 {
		_ = json.Unmarshal(raw, &status)
		status.CurrentRelease = firstNonEmpty(strings.TrimSpace(status.CurrentRelease), strings.TrimSpace(s.releaseID))
	}

	releasesRoot := deploymentReleasesRoot()
	if status.TotalReleaseBytes <= 0 && releasesRoot != "" {
		status.TotalReleaseBytes = directoryBytes(releasesRoot)
	}
	if status.ConsoleGenerationBytes <= 0 {
		status.ConsoleGenerationBytes = directoryBytes(filepath.Join(installDataDir, "console-generations"))
	}
	metrics := sysmetrics.Collect(filepath.Dir(s.cfg.DatabasePath))
	if status.FreeBytes <= 0 {
		status.FreeBytes = int64(metrics.Disk.FreeBytes)
	}
	if status.FreeReserveBytes <= 0 && metrics.Disk.TotalBytes > 0 {
		status.FreeReserveBytes = int64(metrics.Disk.TotalBytes / 10)
		if status.FreeReserveBytes < 512<<20 {
			status.FreeReserveBytes = 512 << 20
		}
	}
	if status.ReleaseBudgetBytes <= 0 {
		status.ReleaseBudgetBytes = status.TotalReleaseBytes * 3
		if status.ReleaseBudgetBytes < 1<<30 {
			status.ReleaseBudgetBytes = 1 << 30
		}
		hardBudget := int64(metrics.Disk.TotalBytes) - status.FreeReserveBytes - status.BackupBytes
		if hardBudget > 0 && status.ReleaseBudgetBytes > hardBudget {
			status.ReleaseBudgetBytes = hardBudget
		}
	}
	status.Draining = readDeploymentReapers(filepath.Join(installDataDir, "run", "reapers"), now)
	for _, reaper := range status.Draining {
		if reaper.HeartbeatAt > status.ReaperHeartbeatAt {
			status.ReaperHeartbeatAt = reaper.HeartbeatAt
		}
		if status.LastReclaimError == "" && reaper.LastError != "" {
			status.LastReclaimError = reaper.LastError
		}
	}
	return status
}

func deploymentInstallDataDir(runtimeDataDir string) string {
	if explicit := strings.TrimSpace(os.Getenv("CODEX_POOL_INSTALL_DATA_DIR")); filepath.IsAbs(explicit) {
		return filepath.Clean(explicit)
	}
	cleaned := filepath.Clean(strings.TrimSpace(runtimeDataDir))
	if filepath.Base(cleaned) == "data" {
		return filepath.Dir(cleaned)
	}
	return cleaned
}

func deploymentReleasesRoot() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	releaseDir := filepath.Dir(executable)
	releasesRoot := filepath.Dir(releaseDir)
	if filepath.Base(releasesRoot) != "releases" {
		return ""
	}
	return releasesRoot
}

func directoryBytes(root string) int64 {
	if strings.TrimSpace(root) == "" {
		return 0
	}
	var total int64
	_ = filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if info, infoErr := entry.Info(); infoErr == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func readDeploymentReapers(root string, now time.Time) []deploymentReaperStatus {
	entries, err := os.ReadDir(root)
	if err != nil {
		return []deploymentReaperStatus{}
	}
	result := make([]deploymentReaperStatus, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(root, entry.Name()))
		if readErr != nil || len(raw) > 64<<10 {
			continue
		}
		var item deploymentReaperStatus
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if item.ReleaseID == "" {
			item.ReleaseID = strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		}
		// Reapers leave a terminal heartbeat briefly for audit/status output. A
		// completed reclaim no longer owns release bytes or requests, and a cancelled
		// reaper means that generation became active again; neither belongs in the
		// API's draining collection or the UI's draining count.
		switch strings.ToLower(strings.TrimSpace(item.State)) {
		case "complete", "cancelled":
			continue
		}
		if item.HeartbeatAt > 0 {
			item.AgeSeconds = now.Unix() - item.HeartbeatAt
			if item.AgeSeconds < 0 {
				item.AgeSeconds = 0
			}
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ReleaseID < result[j].ReleaseID })
	return result
}
