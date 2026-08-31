export type CodexThreadStatus = 'notLoaded' | 'idle' | 'systemError' | 'active' | 'turnAborted' | 'unknown';
export type CodexThreadWaitingReason = 'waitingOnApproval' | 'waitingOnUserInput' | '';

export interface CodexRuntime {
  id: string;
  label?: string;
  generation: number;
  available: boolean;
  lastHeartbeat?: string;
}

// Both fields are opaque. threadHandle is the short-lived capability used for
// reads/mutations; threadKey is a non-authorizing, stable correlation key for
// status events and row selection. Neither contains an app-server raw ID.
export interface CodexThread {
  runtimeId: string;
  runtimeLabel?: string;
  threadKey: string;
  threadHandle: string;
  model?: string;
  modelProvider?: string;
  source?: string;
  status: CodexThreadStatus;
  waitingReason?: CodexThreadWaitingReason;
  activeTurnHandle?: string;
  runtimeAvailable: boolean;
  updatedAt?: string;
  directInput: boolean;
  cwdBase?: string;
  revision: number;
}

export interface CodexThreadFilters {
  runtimeId: string;
  sortKey?: string;
  sortDirection?: string;
  modelProviders?: string[];
  sourceKinds?: string[];
  archived?: boolean;
  isPinned?: boolean;
  searchTerm?: string;
}

export interface CodexThreadPage {
  data: CodexThread[];
  nextCursor?: string;
  backwardsCursor?: string;
}

export interface CodexThreadStatusEvent extends CodexThread {}
