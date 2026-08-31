export interface PlanPresentation {
  plan_family?: string;
  seat_type?: string;
  plan_display_name?: string;
  seat_display_name?: string;
  combined?: string;
}

export function normalizePlanFamily(raw: unknown): string {
  const text = String(raw || '').trim().toLowerCase();
  if (!text) return 'unknown';
  const tokens = text.split(/[^a-z0-9]+/).filter(Boolean);
  if (tokens.includes('enterprise')) return 'enterprise';
  if (tokens.includes('business') || tokens.includes('team') || tokens.includes('teams')) return 'business';
  if (tokens.includes('edu') || tokens.includes('education')) return 'edu';
  if (tokens.includes('api') || tokens.includes('apikey') || tokens.includes('payg') || tokens.includes('paygo')) return 'api';
  if (tokens.includes('pro')) return 'pro';
  if (tokens.includes('plus')) return 'plus';
  if (tokens.includes('free')) return 'free';
  return 'unknown';
}

export function formatPlanLabel(raw: unknown, presentation?: PlanPresentation | null): string {
  const combined = String(presentation?.combined || '').trim();
  if (combined) return combined;
  const displayName = String(presentation?.plan_display_name || '').trim();
  if (displayName) return displayName;
  const family = normalizePlanFamily(raw);
  return ({ business: 'Business / Team', enterprise: 'Enterprise', pro: 'Pro', plus: 'Plus', free: 'Free', edu: 'Education', api: 'API', unknown: 'Unknown' } as Record<string, string>)[family] || 'Unknown';
}

export function formatSeatLabel(raw: unknown): string {
  return ({ business_premium: 'Business Premium (5×)', business_standard: 'Business Standard', enterprise_standard: 'Enterprise Standard', personal: 'Personal', legacy_codex: 'Legacy Codex', unknown: 'Unconfirmed' } as Record<string, string>)[String(raw || 'unknown')] || String(raw || 'Unconfirmed');
}
