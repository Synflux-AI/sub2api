/**
 * Admin API Keys API endpoints
 * Handles API key management for administrators
 */

import { apiClient } from '../client'
import type { ApiKey } from '@/types'

export interface UpdateApiKeyGroupResult {
  api_key: ApiKey
  auto_granted_group_access: boolean
  granted_group_id?: number
  granted_group_name?: string
}

/**
 * Update an API key's group binding
 * @param id - API Key ID
 * @param groupId - Group ID (0 to unbind, positive to bind, null/undefined to skip)
 * @returns Updated API key with auto-grant info
 */
export async function updateApiKeyGroup(id: number, groupId: number | null): Promise<UpdateApiKeyGroupResult> {
  const { data } = await apiClient.put<UpdateApiKeyGroupResult>(`/admin/api-keys/${id}`, {
    group_id: groupId === null ? 0 : groupId
  })
  return data
}

/**
 * 用完整的分组集合替换一把 Key 的绑定（issue #171）。
 *
 * 与 updateApiKeyGroup 的区别：那个只发单个 group_id，语义是遗留的
 * 「现有绑定 <=1 整体替换 / >=2 只改默认组」；这个直接给出集合，是管理端多选的落点。
 *
 * 传空数组 = 解绑全部。**必须照发空数组**，不能因为空就省略字段 ——
 * 省略会让后端回退到单值 group_id 的兼容路径。
 */
export async function updateApiKeyGroups(id: number, groupIds: number[]): Promise<UpdateApiKeyGroupResult> {
  const { data } = await apiClient.put<UpdateApiKeyGroupResult>(`/admin/api-keys/${id}`, {
    group_ids: [...groupIds]
  })
  return data
}

export const apiKeysAPI = {
  updateApiKeyGroup,
  updateApiKeyGroups
}

export default apiKeysAPI
