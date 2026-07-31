/**
 * User-level access token ("sat-…") API endpoints.
 *
 * These endpoints use the panel envelope ({ code, message, data }); the shared
 * apiClient response interceptor already unwraps it, so callers here just get
 * the `data` payload (or throw the normalized `{ status, code, reason, message }`
 * error shape on failure — see `client.ts`).
 */

import { apiClient } from './client'

export interface UserAccessToken {
  token: string | null
  created_at: string | null
  last_used_at: string | null
}

/**
 * Fetch the current user's access token state.
 * `token` is `null` when no token has been generated yet.
 */
export async function getAccessToken(): Promise<UserAccessToken> {
  const { data } = await apiClient.get<UserAccessToken>('/user/access-token')
  return data
}

/**
 * Generate (first time) or rotate (replace) the current user's access token.
 * Requires the account password; wrong password rejects with
 * `reason: 'ACCESS_TOKEN_PASSWORD_INCORRECT'` (403), missing password with
 * `reason: 'PASSWORD_REQUIRED'` (400).
 */
export async function rotateAccessToken(password: string): Promise<UserAccessToken> {
  const { data } = await apiClient.post<UserAccessToken>('/user/access-token/rotate', { password })
  return data
}

/**
 * Revoke the current user's access token. Requires the account password,
 * sent as a request body on the DELETE request.
 */
export async function revokeAccessToken(password: string): Promise<void> {
  await apiClient.delete('/user/access-token', { data: { password } })
}

export const accessTokenAPI = {
  getAccessToken,
  rotateAccessToken,
  revokeAccessToken
}

export default accessTokenAPI
