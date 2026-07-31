/**
 * Admin per-user access token ("sat-…") management endpoints.
 *
 * Unlike the user-facing endpoints in `api/accessToken.ts`, these three
 * routes require no target-user password: an admin can view, generate /
 * rotate, or revoke a user's access token outright. All three are step-up
 * gated on the backend (403 `STEP_UP_REQUIRED` / `STEP_UP_TOTP_NOT_ENABLED` /
 * `STEP_UP_ADMIN_API_KEY_FORBIDDEN` when unsatisfied) — callers should wrap
 * invocations with the existing `useStepUp().run(...)` composable rather than
 * handling step-up ad hoc.
 *
 * `data` is byte-identical in shape to the user-facing endpoint (shared
 * `dto.UserAccessToken` on the backend), so we reuse the same TS type.
 */

import { apiClient } from '../client'
import type { UserAccessToken } from '../accessToken'

export type { UserAccessToken }

/**
 * Fetch a user's access token state. 404s if the user does not exist.
 * `token` is `null` when the user has never generated one.
 */
export async function getUserAccessToken(userId: number): Promise<UserAccessToken> {
  const { data } = await apiClient.get<UserAccessToken>(`/admin/users/${userId}/access-token`)
  return data
}

/**
 * Generate (first time) or rotate (replace) a user's access token.
 * No request body — admin actions require no target-user password.
 */
export async function rotateUserAccessToken(userId: number): Promise<UserAccessToken> {
  const { data } = await apiClient.post<UserAccessToken>(`/admin/users/${userId}/access-token/rotate`)
  return data
}

/**
 * Revoke a user's access token. No request body.
 */
export async function revokeUserAccessToken(userId: number): Promise<void> {
  await apiClient.delete(`/admin/users/${userId}/access-token`)
}

export const accessTokensAPI = {
  getUserAccessToken,
  rotateUserAccessToken,
  revokeUserAccessToken
}

export default accessTokensAPI
