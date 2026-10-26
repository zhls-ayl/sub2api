// Upstream evidence reported by an admin account test (currently Excel BPS):
// the upstream HTTP status plus whatever identifiers the upstream returned.
export interface AccountTestUpstreamEvidence {
  status: number
  model?: string
  requestId?: string
  errorCode?: string
  errorMessage?: string
}
