// Pure legend decision for the simplified charts: pie/doughnut always, line/bar when more than one series,
// an explicit `legend` option wins.
export function legendVisible(type: string, seriesCount: number, explicit?: boolean): boolean {
  if (typeof explicit === 'boolean') return explicit;
  return type === 'pie' || type === 'doughnut' || seriesCount > 1;
}
