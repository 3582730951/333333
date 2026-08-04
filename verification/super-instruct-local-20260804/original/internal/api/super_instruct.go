package api

import (
	"errors"
	"net/http"
	"strings"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/superinstruct"
)

func (s *Server) adminSuperInstructSkills(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	skills, err := s.superInstructLibrary().List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if skills == nil {
		skills = []superinstruct.Skill{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"directory": superinstruct.DefaultDir(),
		"skills":    skills,
	})
}

func (s *Server) adminSuperInstructMemory(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.superMemory == nil {
		writeJSON(w, http.StatusOK, superinstruct.MemoryData{
			Successes:  []superinstruct.SuccessRecord{},
			Patterns:   map[string]uint64{},
			Techniques: map[string]uint64{},
		})
		return
	}
	writeJSON(w, http.StatusOK, s.superMemory.Snapshot())
}

func (s *Server) adminSuperInstructMonitor(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	var memoryCount uint64
	if s.superMemory != nil {
		memoryCount = s.superMemory.SuccessCount()
	}
	if s.superMonitor == nil {
		writeJSON(w, http.StatusOK, superinstruct.MonitorSnapshot{
			Stats:   superinstruct.MonitorStats{MemoryCount: memoryCount},
			History: []superinstruct.InteractionEvent{},
		})
		return
	}
	writeJSON(w, http.StatusOK, s.superMonitor.Snapshot(memoryCount))
}

func (s *Server) validateUserGroupSuperInstructConfig(r *http.Request, group *storage.UserGroup) error {
	if group == nil {
		return nil
	}
	ids, err := superinstruct.NormalizeSkillIDs(group.SuperInstructSkillIDs)
	if err != nil {
		return err
	}
	group.SuperInstructSkillIDs = ids
	profiles, err := normalizeAPISuperInstructProfiles(group.SuperInstructProfiles)
	if err != nil {
		return err
	}
	group.SuperInstructProfiles = profiles
	needsInstructionSkills := group.SuperInstructEnabled
	for _, profile := range profiles {
		if profile.Enabled {
			needsInstructionSkills = true
			break
		}
	}
	if !needsInstructionSkills {
		return nil
	}
	skills, err := s.superInstructLibrary().List(r.Context())
	if err != nil {
		return err
	}
	if len(skills) == 0 {
		return errors.New("Super-Instruct is enabled but no skills are installed")
	}
	usable := 0
	installed := make(map[string]superinstruct.Skill, len(skills))
	for _, skill := range skills {
		installed[skill.ID] = skill
		if skill.Error == "" {
			usable++
		}
	}
	if usable == 0 {
		return errors.New("Super-Instruct is enabled but no valid skills are installed")
	}
	validateIDs := func(ids []string) error {
		for _, id := range ids {
			skill, ok := installed[id]
			if !ok {
				return errors.New("Super-Instruct skill " + id + " is not installed")
			}
			if skill.Error != "" {
				return errors.New("Super-Instruct skill " + id + " is invalid: " + skill.Error)
			}
		}
		return nil
	}
	if group.SuperInstructEnabled {
		if err := validateIDs(ids); err != nil {
			return err
		}
	}
	for family, profile := range profiles {
		if !profile.Enabled {
			continue
		}
		if err := validateIDs(profile.SkillIDs); err != nil {
			return errors.New("Super-Instruct profile " + family + ": " + err.Error())
		}
	}
	return nil
}

func normalizeAPISuperInstructProfiles(profiles storage.SuperInstructProfiles) (storage.SuperInstructProfiles, error) {
	if len(profiles) == 0 {
		return nil, nil
	}
	out := make(storage.SuperInstructProfiles, len(profiles))
	for rawFamily, profile := range profiles {
		family := normalizeAPISuperInstructFamily(rawFamily)
		if family == "" {
			return nil, errors.New("unsupported Super-Instruct profile family " + rawFamily)
		}
		if _, duplicate := out[family]; duplicate {
			return nil, errors.New("duplicate Super-Instruct profile family " + family)
		}
		ids, err := superinstruct.NormalizeSkillIDs(profile.SkillIDs)
		if err != nil {
			return nil, err
		}
		profile.SkillIDs = ids
		out[family] = profile
	}
	return out, nil
}

func normalizeAPISuperInstructFamily(family string) string {
	switch strings.ToLower(strings.TrimSpace(family)) {
	case storage.ModelInstructionFamilyGPT, "chatgpt", "codex", "openai":
		return storage.ModelInstructionFamilyGPT
	case storage.ModelInstructionFamilyClaude, "anthropic":
		return storage.ModelInstructionFamilyClaude
	case storage.ModelInstructionFamilyGemini, "google":
		return storage.ModelInstructionFamilyGemini
	default:
		return ""
	}
}
