/**
 * Admin Model RPM Rules API endpoints
 * 模型维度 RPM 限流：按公开模型名限制每分钟请求数，配额可按用户独立计数或全站共享。
 */

import { apiClient } from '../client'
import type { ModelRPMRule, SaveModelRPMRuleRequest } from '@/types'

export async function list(options?: { signal?: AbortSignal }): Promise<ModelRPMRule[]> {
  const { data } = await apiClient.get<ModelRPMRule[]>('/admin/model-rpm-rules', {
    signal: options?.signal
  })
  return data
}

export async function create(request: SaveModelRPMRuleRequest): Promise<ModelRPMRule> {
  const { data } = await apiClient.post<ModelRPMRule>('/admin/model-rpm-rules', request)
  return data
}

export async function update(
  id: number,
  request: SaveModelRPMRuleRequest
): Promise<ModelRPMRule> {
  const { data } = await apiClient.put<ModelRPMRule>(`/admin/model-rpm-rules/${id}`, request)
  return data
}

export async function deleteRule(id: number): Promise<{ message: string }> {
  const { data } = await apiClient.delete<{ message: string }>(`/admin/model-rpm-rules/${id}`)
  return data
}

const modelRpmRulesAPI = {
  list,
  create,
  update,
  delete: deleteRule
}

export default modelRpmRulesAPI
