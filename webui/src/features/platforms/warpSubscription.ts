// Keep the shortcut and the platform's existing exclusion rules in one state.
export const WARP_EXCLUSION_RULE = "!^WARP/";

export function allowsWarpSubscription(text: string | undefined): boolean {
  return !(text ?? "").split(/\r?\n/).some((line) => line.trim() === WARP_EXCLUSION_RULE);
}

export function setWarpSubscriptionAllowed(text: string | undefined, allowed: boolean): string {
  const lines = (text ?? "").split(/\r?\n/).filter((line) => line.trim() !== WARP_EXCLUSION_RULE);
  if (!allowed) {
    lines.push(WARP_EXCLUSION_RULE);
  }
  return lines.join("\n").replace(/^\n+|\n+$/g, "");
}
