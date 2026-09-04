/*
 * Route → effect plan for P6 wiring.
 *
 * The compositor admits at most ONE effect per composition slot
 * (`SLOT_LIMITS` in effectCompositor.js: base/ambient/interaction/foreground,
 * and at `low` quality only ambient + interaction). Requesting two effects that
 * share a slot is therefore not "two effects": the lower-priority one is
 * silently inactive and looks identical to an effect that failed to render.
 *
 * So every plan below names at most one effect per slot. What the browser shows
 * is then a property of this table rather than of the priority tie-breaks in
 * `compareEntries`, and P7 acceptance can assert it.
 *
 * The full catalogue stays reachable: `scripts/check-effect-rendering.mjs`
 * loads each effect on its own, which is how all 41 are proven to render.
 */

// Signature ambient field + pointer affordance + one material pass. Every route
// starts here and overrides only the slots it has a better use for.
const DEFAULT_PLAN = Object.freeze({
  ambient: 'aurora-gradient',
  interaction: 'cursor-glow',
  foreground: 'noise-grain',
});

const ROUTE_PLANS = Object.freeze({
  // Observability routes read as live data surfaces, so the ambient slot goes to
  // a dataviz field instead of the decorative curtain.
  '/': Object.freeze({ ambient: 'aurora-pulse', interaction: 'cursor-glow', foreground: 'glow-border' }),
  '/usage': Object.freeze({ ambient: 'trend-stroke', interaction: 'cursor-glow', foreground: 'glow-border' }),
  '/quota': Object.freeze({ ambient: 'data-pulse', interaction: 'cursor-glow', foreground: 'glow-border' }),
  '/model-quality': Object.freeze({ ambient: 'trend-stroke', interaction: 'cursor-glow', foreground: 'glow-border' }),
  '/system': Object.freeze({ ambient: 'heat-flow', interaction: 'cursor-glow', foreground: 'glow-border' }),
  '/cf-events': Object.freeze({ ambient: 'gpu-particles', interaction: 'cursor-glow', foreground: 'noise-grain' }),

  // Dense tables: the pointer slot earns more as row affordance than as a halo.
  '/accounts': Object.freeze({ ambient: 'aurora-gradient', interaction: 'hover-parallax-tilt', foreground: 'glow-border' }),
  '/groups': Object.freeze({ ambient: 'force-graph-links', interaction: 'hover-parallax-tilt', foreground: 'glow-border' }),
  '/users': Object.freeze({ ambient: 'aurora-gradient', interaction: 'hover-parallax-tilt', foreground: 'glow-border' }),
  '/keys': Object.freeze({ ambient: 'aurora-gradient', interaction: 'hover-parallax-tilt', foreground: 'glow-border' }),
  '/models': Object.freeze({ ambient: 'breathing-orbs', interaction: 'hover-parallax-tilt', foreground: 'glow-border' }),
  '/audit': Object.freeze({ ambient: 'contour-lines', interaction: 'hover-parallax-tilt', foreground: 'noise-grain' }),
  '/codex-threads': Object.freeze({ ambient: 'grid-wave', interaction: 'hover-parallax-tilt', foreground: 'noise-grain' }),

  // Form-heavy routes: press feedback beats a pointer halo on a page of inputs.
  '/settings-v2': Object.freeze({ ambient: 'aurora-gradient', interaction: 'press-elastic', foreground: 'glass-refraction' }),
  '/registration': Object.freeze({ ambient: 'aurora-gradient', interaction: 'press-elastic', foreground: 'glow-border' }),
  '/team-lifecycle': Object.freeze({ ambient: 'star-parallax', interaction: 'press-elastic', foreground: 'glow-border' }),
  '/egress': Object.freeze({ ambient: 'flowfield-noise', interaction: 'press-elastic', foreground: 'glow-border' }),
  '/providers': Object.freeze({ ambient: 'liquid-metal', interaction: 'magnetic-button', foreground: 'holographic' }),
  '/upstream-error-rules': Object.freeze({ ambient: 'aurora-gradient', interaction: 'press-elastic', foreground: 'noise-grain' }),
  '/email-pool': Object.freeze({ ambient: 'fluid-sim', interaction: 'hover-parallax-tilt', foreground: 'liquid-highlight' }),
  '/email-pool/cloudflare': Object.freeze({ ambient: 'fluid-sim', interaction: 'hover-parallax-tilt', foreground: 'liquid-highlight' }),
  '/public-chat': Object.freeze({ ambient: 'aurora-pulse', interaction: 'click-ripple', foreground: 'depth-fog' }),
  // The six /settings/ai/* routes are one page behind six paths; the prefix
  // walk above resolves all of them to this single entry.
  '/settings/ai': Object.freeze({ ambient: 'aurora-gradient', interaction: 'press-elastic', foreground: 'glass-refraction' }),

  // The user-facing portal is quieter than the admin console by design.
  '/portal': Object.freeze({ ambient: 'aurora-gradient', interaction: 'cursor-glow', foreground: 'paper-texture' }),
  '/portal/keys': Object.freeze({ ambient: 'aurora-gradient', interaction: 'press-elastic', foreground: 'paper-texture' }),
  '/portal/usage': Object.freeze({ ambient: 'trend-stroke', interaction: 'cursor-glow', foreground: 'paper-texture' }),
  '/portal/quota': Object.freeze({ ambient: 'data-pulse', interaction: 'cursor-glow', foreground: 'paper-texture' }),
  '/portal/models': Object.freeze({ ambient: 'breathing-orbs', interaction: 'hover-parallax-tilt', foreground: 'paper-texture' }),
  '/portal/profile': Object.freeze({ ambient: 'aurora-gradient', interaction: 'press-elastic', foreground: 'paper-texture' }),
  '/portal/sessions': Object.freeze({ ambient: 'aurora-gradient', interaction: 'hover-parallax-tilt', foreground: 'paper-texture' }),
});

// Slots the plan is allowed to name, in the order the compositor sorts them.
export const PLAN_SLOTS = Object.freeze(['base', 'ambient', 'interaction', 'foreground']);

/**
 * Resolves a pathname to its plan. A nested route falls back to its closest
 * declared ancestor, but `/` is deliberately NOT treated as everyone's ancestor:
 * it is the Dashboard's own plan, and an unrecognised route inheriting the
 * Dashboard's dataviz pulse would be a silent wrong answer rather than a default.
 */
export function planForRoute(pathname) {
  const path = typeof pathname === 'string' && pathname ? pathname : '/';
  if (ROUTE_PLANS[path]) return ROUTE_PLANS[path];
  let candidate = path;
  while (candidate.length > 1) {
    const slash = candidate.lastIndexOf('/');
    if (slash <= 0) break;
    candidate = candidate.slice(0, slash);
    if (ROUTE_PLANS[candidate]) return ROUTE_PLANS[candidate];
  }
  return DEFAULT_PLAN;
}

/**
 * The plan as the `effects` array `createEngineHost` accepts. Order is the slot
 * order above so a reader of the host diagnostics sees a stable sequence.
 */
export function effectsForRoute(pathname) {
  const plan = planForRoute(pathname);
  const effects = [];
  for (let index = 0; index < PLAN_SLOTS.length; index += 1) {
    const id = plan[PLAN_SLOTS[index]];
    if (id) effects.push({ id, parameters: {} });
  }
  return effects;
}

export { DEFAULT_PLAN, ROUTE_PLANS };
