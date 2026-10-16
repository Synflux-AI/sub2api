<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import HelpTooltip from '@/components/common/HelpTooltip.vue'

// 账号级「禁止透传的请求头」（#300）：按行编辑，保存时由调用方规范化后写入 extra。
const model = defineModel<string>({ default: '' })
const { t } = useI18n()
</script>

<template>
  <div>
    <div class="flex items-center">
      <label class="input-label">{{ t('admin.accounts.clientHeaderPassthroughBlocklist') }}</label>
      <HelpTooltip width-class="w-80">
        <p>{{ t('admin.accounts.clientHeaderPassthroughBlocklistHelp') }}</p>
      </HelpTooltip>
    </div>
    <textarea
      v-model="model"
      data-testid="client-header-passthrough-blocklist"
      rows="2"
      class="input font-mono text-sm"
      :placeholder="t('admin.accounts.clientHeaderPassthroughBlocklistPlaceholder')"
    />
  </div>
</template>
