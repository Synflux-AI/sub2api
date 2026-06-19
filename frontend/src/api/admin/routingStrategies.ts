/**
 * Admin Routing Strategies API endpoints
 * 智能路由策略：按模型 / 客户端 / User-Agent 将请求路由到指定账号。
 */

import { apiClient } from '../client'
import type {
  RoutingStrategy,
  SaveRoutingStrategyRequest,
  TestRoutingStrategyRequest,
  TestRoutingStrategyResult
} from '@/types'

export async function list(options?: { signal?: AbortSignal }): Promise<RoutingStrategy[]> {
  const { data } = await apiClient.get<RoutingStrategy[]>('/admin/routing-strategies', {
    signal: options?.signal
  })
  return data
}

export async function getById(id: number): Promise<RoutingStrategy> {
  const { data } = await apiClient.get<RoutingStrategy>(`/admin/routing-strategies/${id}`)
  return data
}

export async function create(request: SaveRoutingStrategyRequest): Promise<RoutingStrategy> {
  const { data } = await apiClient.post<RoutingStrategy>('/admin/routing-strategies', request)
  return data
}

export async function update(
  id: number,
  request: SaveRoutingStrategyRequest
): Promise<RoutingStrategy> {
  const { data } = await apiClient.put<RoutingStrategy>(`/admin/routing-strategies/${id}`, request)
  return data
}

export async function deleteStrategy(id: number): Promise<{ message: string }> {
  const { data } = await apiClient.delete<{ message: string }>(`/admin/routing-strategies/${id}`)
  return data
}

export async function test(
  request: TestRoutingStrategyRequest
): Promise<TestRoutingStrategyResult> {
  const { data } = await apiClient.post<TestRoutingStrategyResult>(
    '/admin/routing-strategies/test',
    request
  )
  return data
}

const routingStrategiesAPI = {
  list,
  getById,
  create,
  update,
  delete: deleteStrategy,
  test
}

export default routingStrategiesAPI
