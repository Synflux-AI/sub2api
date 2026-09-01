export default {
  modelRpmRules: {
    title: '模型限流',
    description:
      '按公开模型名限制每分钟请求数。命中的规则全部生效，任一超限即拒绝，超限返回 429 与 Retry-After。',

    createButton: '新建规则',
    createTitle: '新建模型限流规则',
    editTitle: '编辑模型限流规则',
    allRulesApplyHint:
      '命中的规则全部生效：给「全部用户」配了限额后，再给某个用户配更宽的额度也不会放宽——更宽的规则不会覆盖更严的规则。',
    noRulesYet: '还没有模型限流规则',
    createFirstRule: '创建第一条规则，按模型名限制每分钟请求数。',
    loadError: '加载模型限流规则失败',
    saveSuccess: '规则已保存',
    saveFailed: '保存规则失败',
    deleteSuccess: '规则已删除',
    deleteConfirm: '确定删除规则「{name}」吗？',

    columns: {
      name: '规则名',
      modelPattern: '模型匹配',
      scope: '配额口径',
      target: '适用范围',
      rpmLimit: '每分钟上限',
      enabled: '启用',
      actions: '操作',
    },

    scopes: {
      user: '每用户独立',
      global: '全站共享',
    },

    targetTypes: {
      all: '全部用户',
      group: '分组',
      user: '用户',
    },

    fields: {
      name: '规则名',
      namePlaceholder: '便于在管理台辨认，例如「opus 全站池」',
      modelPattern: '模型匹配',
      modelPatternHint: '客户端请求体里的公开模型名，大小写不敏感；支持尾部 * 前缀通配，例如 claude-opus-*。',
      scope: '配额口径',
      scopeHint: '「每用户独立」= 每个用户各有一份配额；「全站共享」= 适用范围内所有用户共用一个池。',
      scopeLockedHint: '适用范围是单个用户时，两种口径效果相同，已锁定为「每用户独立」。',
      targetType: '适用范围',
      targetTypeHint: '适用范围决定这条规则管谁，配额口径决定配额怎么分，两者相互独立。',
      targetGroup: '目标分组',
      selectGroup: '请选择分组',
      targetUser: '目标用户',
      searchUserPlaceholder: '搜索用户名或邮箱',
      rpmLimit: '每分钟上限',
      rpmLimitHint: '必须为正整数。注意与分组的「用户专属 RPM」不同：那里的 0 表示免检，这里不允许填 0。',
      enabled: '启用该规则',
    },

    errors: {
      nameRequired: '请填写规则名',
      modelPatternRequired: '请填写模型匹配',
      modelPatternWildcard: '通配符只能出现在末尾，且不能只填一个 *',
      targetRequired: '请选择适用范围的目标',
      rpmLimitPositive: '每分钟上限必须为正整数',
    },
  },
}
