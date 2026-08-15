export default {
  smartRouting: {
    title: 'Smart Routing',
    description:
      'Single entry point for channel account scheduling: inspect scheduling state, manage routing strategies, and adjust scheduling settings.',

    tabs: {
      overview: 'Overview',
      accounts: 'Channel Accounts',
      strategies: 'Routing Strategies',
      settings: 'Scheduling Settings',
    },

    common: {
      refresh: 'Refresh',
      loading: 'Loading…',
      never: 'Never',
      unlimited: 'Unlimited',
      all: 'All',
      none: 'None',
      unknown: 'Unknown',
      viewAll: 'View all',
      goToAccount: 'Go to Accounts',
      updatedAt: 'As of',
    },

    // ---------- Tab 1 Overview ----------
    overview: {
      switchesTitle: 'Scheduling Switches',
      switchesHint:
        'These switches control whether soft signals take part in account selection. Health scores only reorder candidates — they never remove an account from the candidate set.',

      healthScoring: 'Account Health Score',
      healthScoringOff: 'Off',
      healthScoringShadow: 'Shadow mode',
      healthScoringActive: 'Active',
      healthScoringOffHint: 'Not collected, not used for ordering',
      healthScoringShadowHint: 'Collected and recorded only, does not affect selection',
      healthScoringActiveHint: 'Used for candidate-pool tiering and ordering',

      stickyBreak: 'Sticky Break',
      stickyBreakHint: 'Whether to switch accounts when a sticky session drops to the probation tier',

      priceAware: 'Price-Aware Scheduling',
      priceAwareHint: 'Selects by a combined price × load score within the same tier and priority',

      openaiAdvanced: 'OpenAI Advanced Scheduler',
      openaiAdvancedHint: 'Weighted multi-factor scoring followed by weighted random selection',

      enabled: 'Enabled',
      disabled: 'Disabled',

      countsTitle: 'Account Availability',
      countsHint:
        'Counted from the current hard-gate state. Hard gates remove accounts from the candidate set — unlike health scores, which are soft signals.',
      totalAccounts: 'Total Accounts',
      schedulable: 'Schedulable',
      manuallyDisabled: 'Manually Disabled',
      inactive: 'Inactive / Error',
      rateLimited: 'Rate Limited (429)',
      overloaded: 'Overloaded (529)',
      tempUnschedulable: 'Temporarily Unschedulable',

      byPlatformTitle: 'By Platform',
      byGroupTitle: 'By Group',
      platform: 'Platform',
      group: 'Group',
      ungrouped: 'Ungrouped',
      total: 'Total',
      available: 'Schedulable',

      blockedTitle: 'Currently Unschedulable Accounts',
      blockedHint:
        'Sorted by recovery time. Entries without a recovery time need manual intervention.',
      blockedEmpty: 'No accounts are currently blocked by a gate',
      account: 'Account',
      reason: 'Reason',
      recoversIn: 'Recovers In',
      needsManualAction: 'Manual action required',

      reasonInactive: 'Status error',
      reasonManual: 'Manually disabled',
      reasonRateLimit: 'Rate limited (429)',
      reasonOverload: 'Overloaded (529)',
      reasonTempUnsched: 'Temporarily unschedulable',
    },

    // ---------- Tab 2 Channel Accounts ----------
    accounts: {
      searchPlaceholder: 'Search account name…',
      filterPlatform: 'Platform',
      filterGroup: 'Group',
      filterStatus: 'Scheduling status',
      statusAll: 'All',
      statusAvailable: 'Schedulable',
      statusBlocked: 'Blocked',

      colAccount: 'Account',
      colPlatform: 'Platform',
      colGroups: 'Groups',
      colGate: 'Gate Status',
      colHealth: 'Health',
      colPriority: 'Priority',
      colLoad: 'Load',
      colRuntime: 'Runtime',
      colLastUsed: 'Last Used',
      colActions: 'Actions',

      gateAvailable: 'Schedulable',
      modelRateLimited: '429 · {model} · {countdown}',
      healthDisabled: 'Disabled',
      healthTier0: 'Primary',
      healthTier1: 'Candidate',
      healthTier2: 'Probation',
      healthTier0Hint: 'Participates in scheduling normally',
      healthTier1Hint: 'Used only when all primary-pool accounts are unavailable or busy',
      healthTier2Hint: 'Near circuit-break; used only when every other account is unavailable',
      healthSoftSignalHint:
        'The health score is a soft signal — it only affects ordering and never removes an account from the candidate set',

      concurrency: 'Concurrency',
      rpm: 'RPM',
      windowCost: 'Window cost',
      sessions: 'Sessions',

      empty: 'No accounts match the current filters',
      editAccount: 'Edit account',
      editAccountHint: 'Credentials, proxies and other non-scheduling settings live in Accounts',
    },

    // ---------- Tab 4 Scheduling Settings ----------
    settings: {
      savedSuccess: 'Saved',
      saveFailed: 'Save failed',
      save: 'Save',
      saving: 'Saving…',
      reload: 'Reload',

      healthTitle: 'Account Health Score',
      healthHint:
        'Health scores are driven by upstream error and success events (0–100, decaying back to 100 by half-life) and only reorder accounts within the candidate set. Thresholds, penalty weights and half-life are currently config.yaml-only.',
      healthScoringEnabled: 'Enable health scoring',
      healthScoringEnabledHint: 'Master switch: collection + ordering',
      healthShadowMode: 'Shadow mode',
      healthShadowModeHint:
        'Collect and record scores only, without affecting selection. Recommended before going live.',
      healthStickyBreak: 'Break stickiness on probation tier',
      healthStickyBreakHint:
        'Breaking stickiness loses the prompt cache, so it only triggers when an account is near circuit-break',

      priceTitle: 'Price-Aware Scheduling',
      priceHint:
        'When enabled, accounts within the same health tier and priority are selected by a combined price × load score. Weight and load-guard threshold are configured in config.yaml.',
      priceAwareEnabled: 'Enable price-aware scheduling',

      openaiTitle: 'OpenAI Advanced Scheduler Weights',
      openaiHint:
        'The OpenAI path uses an independent weighted scoring model, parallel to the health-score system above. "Effective" shows the value the backend actually uses.',
      openaiAdvancedEnabled: 'Enable advanced scheduler',
      openaiStickyWeighted: 'Sticky weighting',
      openaiSubscriptionPriority: 'Prefer subscription accounts',
      openaiLbTopK: 'Load-balancing candidates (top-K)',
      weightPriority: 'Priority weight',
      weightLoad: 'Load weight',
      weightQueue: 'Queue weight',
      weightErrorRate: 'Error-rate weight',
      weightTtft: 'TTFT weight',
      weightReset: 'Reset-proximity weight',
      weightQuotaHeadroom: 'Quota-headroom weight',
      weightUpstreamCost: 'Upstream-cost weight',
      weightPreviousResponse: 'Context-affinity weight',
      weightSessionSticky: 'Session-stickiness weight',
      effectiveValue: 'Effective',

      cooldownTitle: 'Cooldowns and Gates',
      cooldownHint:
        'These are hard gates: once triggered, the account leaves the candidate set until the cooldown ends.',
      cooldown429: '429 rate-limit cooldown',
      cooldown529: '529 overload cooldown',
      streamTimeout: 'Stream timeout handling',
      tempUnschedGuard: 'Disable temporary unscheduling',
      tempUnschedGuardHint:
        'When on, the gateway no longer auto-unschedules Anthropic / OpenAI accounts on upstream errors',
      configuredIn: 'Configured in System Settings',
      openInSettings: 'Open settings',

      otherTitle: 'Other Scheduling Settings',
      allowUngroupedKey: 'Allow ungrouped API key scheduling',
      allowUngroupedKeyHint: 'When off, ungrouped keys receive a 403',
      errorHandlingRules: 'Error Handling Rules',
      errorHandlingRulesHint:
        'Automatically unschedule, switch accounts and more based on upstream error patterns',
      openErrorRules: 'Open error handling rules',

      configOnlyNotice:
        'Parameters with this badge can currently only be changed in config.yaml and require a restart. In-page overrides are planned.',
      configOnly: 'config.yaml only',
    },
  },
}
