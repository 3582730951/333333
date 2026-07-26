import { useQuery } from '@tanstack/react-query';
import {
  applySettingsTemplate, clearContextJournal, clearLogRecords, fetchAdvancedSettings, fetchAIConfigSettings, fetchAutomationSettings, fetchConfigSettings, fetchLifecycleSettings,
  fetchLoggingSettings, fetchMemorySettings, fetchRegistrarSettings, fetchSharedSettingsOptions,
  saveAdvancedSettings, saveRegistrarSettings, saveSettingsPatches,
} from '../api/settings';
import type { AdvancedSettingsKind, AdvancedSettingsSaveInput, AISettingsDomain, RegistrarSaveInput, SettingsPatch, SettingsSection } from '../model/settings';
import { queryKeys, useApiMutation, useQueryView } from '../../shared/queries';

export const settingsQueryKeys = {
  all: queryKeys.domain('settings'),
  section: (section: SettingsSection) => queryKeys.list('settings', { section }),
  advanced: (kind: AdvancedSettingsKind) => queryKeys.list('settings', { advanced: kind }),
  ai: (domain: AISettingsDomain) => queryKeys.list('settings', { placement: 'ai_settings', domain }),
  sharedOptions: queryKeys.list('settings', { resource: 'shared-options' }),
};

function useSettingsQuery<T>(section: SettingsSection, queryFn: (signal?: AbortSignal) => Promise<T>) {
  return useQueryView(useQuery({
    queryKey: settingsQueryKeys.section(section),
    queryFn: ({ signal }) => queryFn(signal),
  }));
}

export function useConfigSettingsData() {
  return useSettingsQuery('config', fetchConfigSettings);
}

export function useAIConfigSettingsData(domain: AISettingsDomain) {
  return useQueryView(useQuery({
    queryKey: settingsQueryKeys.ai(domain),
    queryFn: ({ signal }) => fetchAIConfigSettings(domain, signal),
  }));
}

export function useAutomationSettingsData() {
  return useSettingsQuery('automation', fetchAutomationSettings);
}

export function useRegistrarSettingsData() {
  return useSettingsQuery('registrar', fetchRegistrarSettings);
}

export function useLifecycleSettingsData() {
  return useSettingsQuery('lifecycle', fetchLifecycleSettings);
}

export function useLoggingSettingsData() {
  return useSettingsQuery('logging', fetchLoggingSettings);
}

export function useMemorySettingsData() {
  return useSettingsQuery('memory', fetchMemorySettings);
}

export function useSharedSettingsOptions(enabled: boolean) {
  return useQueryView(useQuery({
    queryKey: settingsQueryKeys.sharedOptions,
    queryFn: ({ signal }) => fetchSharedSettingsOptions(signal),
    enabled,
  }));
}

export function useAdvancedSettingsData(kind: AdvancedSettingsKind) {
  return useQueryView(useQuery({
    queryKey: settingsQueryKeys.advanced(kind),
    queryFn: ({ signal }) => fetchAdvancedSettings(kind, signal),
  }));
}

export function useSaveSettingsMutation() {
  return useApiMutation<SettingsPatch[], Awaited<ReturnType<typeof saveSettingsPatches>>>({
    mutationFn: saveSettingsPatches,
    invalidate: [settingsQueryKeys.all],
  });
}

export function useClearLogRecordsMutation() {
  return useApiMutation<void, Awaited<ReturnType<typeof clearLogRecords>>>({
    mutationFn: clearLogRecords,
  });
}

export function useClearContextJournalMutation() {
  return useApiMutation<void, Awaited<ReturnType<typeof clearContextJournal>>>({
    mutationFn: clearContextJournal,
  });
}

export function useApplySettingsTemplateMutation() {
  return useApiMutation<string, Awaited<ReturnType<typeof applySettingsTemplate>>>({
    mutationFn: applySettingsTemplate,
    invalidate: [settingsQueryKeys.all],
  });
}

export function useSaveRegistrarMutation() {
  return useApiMutation<RegistrarSaveInput, Awaited<ReturnType<typeof saveRegistrarSettings>>>({
    mutationFn: saveRegistrarSettings,
    invalidate: [settingsQueryKeys.all],
  });
}

export function useSaveAdvancedSettingsMutation() {
  return useApiMutation<AdvancedSettingsSaveInput, Awaited<ReturnType<typeof saveAdvancedSettings>>>({
    mutationFn: saveAdvancedSettings,
    invalidate: [settingsQueryKeys.all],
  });
}
