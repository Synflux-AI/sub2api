<template>
  <div ref="containerRef" class="relative">
    <button
      ref="triggerRef"
      type="button"
      class="select-trigger flex w-full cursor-pointer items-center justify-between gap-2 text-left"
      :class="isOpen ? 'select-trigger-open' : ''"
      :aria-expanded="isOpen"
      aria-haspopup="listbox"
      :aria-label="ariaLabel"
      :title="triggerLabel"
      @click="toggleOpen"
    >
      <!--
        selected 插槽可选：不传时回退到纯文本 triggerLabel（原有行为逐字不变）。
        加它是为了让分组多选能复用 GroupBadge 之类的富渲染，而不必再造一个组件。
      -->
      <span class="select-value min-w-0 flex-1 truncate">
        <slot name="selected" :options="selectedOptions" :label="triggerLabel">{{ triggerLabel }}</slot>
      </span>
      <span v-if="modelValue.length > 1" class="shrink-0 text-xs text-gray-400 dark:text-dark-400">({{ modelValue.length }})</span>
      <span class="select-icon shrink-0 text-gray-400 transition-transform" :class="isOpen ? 'rotate-180' : ''">
        <Icon name="chevronDown" size="md" />
      </span>
    </button>

    <Teleport to="body">
      <Transition name="select-dropdown">
        <div
          v-if="isOpen"
          ref="dropdownRef"
          class="select-dropdown-portal"
          :class="[instanceId]"
          :style="dropdownStyle"
          role="listbox"
          aria-multiselectable="true"
          @click.stop
          @mousedown.stop
        >
          <div v-if="isSearchable" class="select-search">
            <Icon name="search" size="sm" class="text-gray-400" />
            <input
              ref="searchInputRef"
              v-model="searchQuery"
              type="text"
              class="select-search-input"
              :placeholder="searchPlaceholder"
              :aria-label="searchPlaceholder"
              @click.stop
            />
          </div>

          <div class="select-options">
            <button
              type="button"
              role="option"
              class="select-option w-full border-b border-gray-100 text-left font-medium dark:border-dark-700"
              :class="isExclusiveSelected ? 'select-option-selected' : ''"
              :aria-selected="isExclusiveSelected"
              @click="selectExclusive"
            >
              <span class="select-option-label">{{ exclusiveEmptyLabel }}</span>
              <Icon v-if="isExclusiveSelected" name="check" size="sm" class="text-primary-500" />
            </button>

            <button
              v-for="option in filteredOptions"
              :key="`${typeof option.value}:${option.value}`"
              type="button"
              role="option"
              class="select-option w-full text-left"
              :class="[
                isSelected(option.value) ? 'select-option-selected' : '',
                option.disabled ? 'select-option-disabled' : '',
              ]"
              :disabled="option.disabled"
              :aria-selected="isSelected(option.value)"
              :aria-disabled="!!option.disabled"
              @click="toggleOption(option)"
            >
              <!-- option 插槽可选：不传时回退到纯文本 label（原有行为逐字不变）。 -->
              <span class="select-option-label">
                <slot name="option" :option="option" :selected="isSelected(option.value)">{{ option.label }}</slot>
              </span>
              <Icon v-if="isSelected(option.value)" name="check" size="sm" class="text-primary-500" />
            </button>

            <div v-if="filteredOptions.length === 0" class="select-empty">{{ noResultsText }}</div>
          </div>
        </div>
      </Transition>
    </Teleport>
  </div>
</template>

<script setup lang="ts">
import type { CSSProperties } from 'vue'
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import Icon from '@/components/icons/Icon.vue'

export interface MultiSelectOption {
  value: number | string
  label: string
  disabled?: boolean
}

const props = withDefaults(
  defineProps<{
    modelValue: (number | string)[]
    options: MultiSelectOption[]
    placeholder?: string
    /** 'auto' 显示搜索框，条件与 Select.vue 一致：选项数 > 5 时显示。 */
    searchable?: boolean | 'auto'
    /** 首项互斥选项的文案；勾选它等价于清空 modelValue（空数组）。 */
    exclusiveEmptyLabel?: string
    /** 搜索无匹配结果时的占位文案；由调用方传入已本地化文案，默认值只是英文兜底字面量。 */
    noResultsText?: string
    /** trigger 的无障碍名称；由调用方传入已本地化文案，默认值只是英文兜底字面量。 */
    ariaLabel?: string
    /** 搜索框的 placeholder 兼 aria-label；由调用方传入已本地化文案，默认值只是英文兜底字面量。 */
    searchPlaceholder?: string
  }>(),
  {
    placeholder: '',
    searchable: 'auto',
    exclusiveEmptyLabel: '',
    noResultsText: 'No results',
    ariaLabel: 'Select options',
    searchPlaceholder: 'Search',
  },
)

const emit = defineEmits<{ 'update:modelValue': [value: (number | string)[]] }>()

const containerRef = ref<HTMLElement | null>(null)
const triggerRef = ref<HTMLButtonElement | null>(null)
const dropdownRef = ref<HTMLElement | null>(null)
const searchInputRef = ref<HTMLInputElement | null>(null)
const isOpen = ref(false)
const searchQuery = ref('')
const instanceId = `multi-select-${Math.random().toString(36).slice(2, 9)}`

const isSelected = (value: number | string) => props.modelValue.includes(value)
const isExclusiveSelected = computed(() => props.modelValue.length === 0)

const isSearchable = computed(() => {
  if (props.searchable === 'auto') return props.options.length > 5
  return props.searchable
})

const filteredOptions = computed(() => {
  if (!isSearchable.value || !searchQuery.value.trim()) return props.options
  const query = searchQuery.value.trim().toLowerCase()
  return props.options.filter((option) => option.label.toLowerCase().includes(query))
})

const labelByValue = computed(() => {
  const map = new Map<number | string, string>()
  for (const option of props.options) map.set(option.value, option.label)
  return map
})

// options 里查不到的值（如已被软删除的分组这类悬空 ID）回退成 `#<value>`，
// 与列表列的展示保持一致；不能渲染成空串，否则 trigger 会看起来像「全局」。
const selectedLabels = computed(() =>
  props.modelValue.map((value) => labelByValue.value.get(value) ?? `#${value}`),
)

// selectedOptions 按 modelValue 的顺序给出已选中的完整 option 对象，
// 供 selected 插槽做富渲染（纯文本回退用不到它）。
const selectedOptions = computed(() =>
  props.modelValue
    .map((value) => props.options.find((option) => option.value === value))
    .filter((option): option is MultiSelectOption => option !== undefined),
)

// trigger 的可见文案与 title 完全一致，共用同一个 computed。
const triggerLabel = computed(() => {
  if (props.modelValue.length === 0) return props.exclusiveEmptyLabel || props.placeholder
  return selectedLabels.value.join('、')
})

const dropdownStyle = computed<CSSProperties>(() => {
  const trigger = triggerRef.value
  if (!trigger) return {}
  const rect = trigger.getBoundingClientRect()
  const padding = 8
  const viewportRight = Math.max(padding, window.innerWidth - padding)
  const left = Math.min(Math.max(padding, rect.left), viewportRight)
  const availableWidth = Math.max(0, viewportRight - left)
  const preferredMinWidth = Math.max(200, rect.width)
  const minWidth = Math.min(preferredMinWidth, availableWidth)
  return {
    position: 'fixed' as const,
    left: `${left}px`,
    top: `${rect.bottom + 4}px`,
    minWidth: `${minWidth}px`,
    maxWidth: `${availableWidth}px`,
    zIndex: '100000020',
  }
})

function selectExclusive() {
  emit('update:modelValue', [])
  // 保持打开，方便连续操作。
}

function toggleOption(option: MultiSelectOption) {
  if (option.disabled) return
  const selected = new Set(props.modelValue)
  if (selected.has(option.value)) selected.delete(option.value)
  else selected.add(option.value)
  emit('update:modelValue', [...selected])
  // 保持打开（多选语义）。
}

function toggleOpen() {
  isOpen.value ? close() : open()
}

function open() {
  isOpen.value = true
  void nextTick(() => {
    positionDropdown()
    if (isSearchable.value) searchInputRef.value?.focus()
  })
}

function close() {
  isOpen.value = false
  searchQuery.value = ''
}

function positionDropdown() {
  const trigger = triggerRef.value
  const dropdown = dropdownRef.value
  if (!trigger || !dropdown) return
  const rect = trigger.getBoundingClientRect()
  const padding = 8
  const viewportRight = Math.max(padding, window.innerWidth - padding)
  const left = Math.min(Math.max(padding, rect.left), viewportRight)
  const availableWidth = Math.max(0, viewportRight - left)
  const preferredMinWidth = Math.max(200, rect.width)
  const minWidth = Math.min(preferredMinWidth, availableWidth)
  dropdown.style.left = `${left}px`
  dropdown.style.top = `${rect.bottom + 4}px`
  dropdown.style.minWidth = `${minWidth}px`
  dropdown.style.maxWidth = `${availableWidth}px`
}

function onDocumentMouseDown(event: MouseEvent) {
  if (!isOpen.value) return
  const target = event.target as Node | null
  if (!target) return
  if (containerRef.value?.contains(target)) return
  if (dropdownRef.value?.contains(target)) return
  close()
}

function onDocumentKeyDown(event: KeyboardEvent) {
  if (event.key === 'Escape') close()
}

function onWindowChange() {
  if (isOpen.value) positionDropdown()
}

watch(isOpen, (open) => {
  if (open) {
    document.addEventListener('mousedown', onDocumentMouseDown)
    document.addEventListener('keydown', onDocumentKeyDown)
    window.addEventListener('resize', onWindowChange)
    window.addEventListener('scroll', onWindowChange, true)
  } else {
    document.removeEventListener('mousedown', onDocumentMouseDown)
    document.removeEventListener('keydown', onDocumentKeyDown)
    window.removeEventListener('resize', onWindowChange)
    window.removeEventListener('scroll', onWindowChange, true)
  }
})

onBeforeUnmount(() => {
  document.removeEventListener('mousedown', onDocumentMouseDown)
  document.removeEventListener('keydown', onDocumentKeyDown)
  window.removeEventListener('resize', onWindowChange)
  window.removeEventListener('scroll', onWindowChange, true)
})
</script>

<style scoped>
/* select-trigger* 与 Select.vue 里的定义一致但为 scoped 样式，需要在这里复制一份才能生效
   （做法与 features/channel-monitor-v2/FilterMultiSelect.vue 相同）。select-dropdown-portal
   及其内部 select-search / select-options / select-option* 类是 Select.vue 里的全局样式
   （未加 scoped），已随构建产物全局生效，这里直接复用，不新增任何样式规则。 */
.select-trigger {
  @apply flex w-full items-center justify-between gap-2;
  @apply rounded-xl px-4 py-2.5 text-sm;
  @apply bg-white dark:bg-dark-800;
  @apply border border-gray-200 dark:border-dark-600;
  @apply text-gray-900 dark:text-gray-100;
  @apply transition-all duration-200;
  @apply focus:border-primary-500 focus:outline-none focus:ring-2 focus:ring-primary-500/30;
  @apply hover:border-gray-300 dark:hover:border-dark-500;
  @apply cursor-pointer;
}

.select-trigger-open {
  @apply border-primary-500 ring-2 ring-primary-500/30;
}
</style>
