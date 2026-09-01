export default {
  modelRpmRules: {
    title: 'Model RPM Limits',
    description:
      'Cap requests per minute by public model name. Every matching rule applies and any breach rejects the request with 429 plus Retry-After.',

    createButton: 'New rule',
    createTitle: 'New model RPM rule',
    editTitle: 'Edit model RPM rule',
    allRulesApplyHint:
      'Every matching rule applies: once a limit exists for "All users", giving one user a higher limit does NOT relax it — a wider rule never overrides a stricter one.',
    noRulesYet: 'No model RPM rules yet',
    createFirstRule: 'Create the first rule to cap requests per minute for a model.',
    loadError: 'Failed to load model RPM rules',
    saveSuccess: 'Rule saved',
    saveFailed: 'Failed to save rule',
    deleteSuccess: 'Rule deleted',
    deleteConfirm: 'Delete rule "{name}"?',

    columns: {
      name: 'Name',
      modelPattern: 'Model pattern',
      scope: 'Quota scope',
      target: 'Applies to',
      rpmLimit: 'Limit / min',
      enabled: 'Enabled',
      actions: 'Actions',
    },

    scopes: {
      user: 'Per user',
      global: 'Shared pool',
    },

    targetTypes: {
      all: 'All users',
      group: 'Group',
      user: 'User',
    },

    fields: {
      name: 'Name',
      namePlaceholder: 'Label shown in the admin console, e.g. "opus shared pool"',
      modelPattern: 'Model pattern',
      modelPatternHint:
        'Public model name from the client request body, case-insensitive. A single trailing * matches by prefix, e.g. claude-opus-*.',
      scope: 'Quota scope',
      scopeHint:
        '"Per user" gives every user their own quota; "Shared pool" makes everyone in range share one bucket.',
      scopeLockedHint:
        'With a single user as the target both scopes behave identically, so this is locked to "Per user".',
      targetType: 'Applies to',
      targetTypeHint:
        'The target decides who the rule governs; the scope decides how the quota is split. They are independent.',
      targetGroup: 'Target group',
      selectGroup: 'Select a group',
      targetUser: 'Target user',
      searchUserPlaceholder: 'Search by username or email',
      rpmLimit: 'Limit per minute',
      rpmLimitHint:
        'Must be a positive integer. Unlike a group per-user RPM override, 0 is not accepted here — there is no bypass value.',
      enabled: 'Enable this rule',
    },

    errors: {
      nameRequired: 'Name is required',
      modelPatternRequired: 'Model pattern is required',
      modelPatternWildcard: 'The wildcard may only appear at the end, and cannot be the whole pattern',
      targetRequired: 'Select the target this rule applies to',
      rpmLimitPositive: 'Limit per minute must be a positive integer',
    },
  },
}
