export default {
  smartRouting: {
    title: '智能调度',
    description: '渠道账号调度的统一入口：查看调度状态、管理路由策略、调整调度配置。',

    tabs: {
      overview: '总览',
      accounts: '渠道账号',
      strategies: '路由策略',
      settings: '调度设置',
    },

    common: {
      refresh: '刷新',
      loading: '加载中…',
      never: '从未',
      unlimited: '不限',
      all: '全部',
      none: '无',
      unknown: '未知',
      viewAll: '查看全部',
      goToAccount: '前往账号管理',
      updatedAt: '数据时间',
    },

    // ---------- Tab 1 总览 ----------
    overview: {
      switchesTitle: '调度开关',
      switchesHint: '这些开关决定了软信号是否参与选号。健康分只改变账号排序，不会把账号移出候选集。',

      healthScoring: '账号健康分',
      healthScoringOff: '已关闭',
      healthScoringShadow: '影子模式',
      healthScoringActive: '已生效',
      healthScoringOffHint: '不采集、不排序',
      healthScoringShadowHint: '只采集记录，不影响选号',
      healthScoringActiveHint: '参与候选池分层排序',

      stickyBreak: '粘性打破',
      stickyBreakHint: '账号跌入隔离观察层时是否更换粘性会话账号',

      priceAware: '价格感知调度',
      priceAwareHint: '同层同优先级内按「价格 × 负载」综合分选择',

      openaiAdvanced: 'OpenAI 高级调度',
      openaiAdvancedHint: '按多维权重打分后加权随机选号',

      enabled: '开启',
      disabled: '关闭',

      countsTitle: '账号可调度性',
      countsHint: '按当前时刻的硬闸门状态统计。硬闸门会把账号移出候选集，与健康分（软信号）性质不同。',
      totalAccounts: '账号总数',
      schedulable: '可调度',
      manuallyDisabled: '手动停用',
      inactive: '非活跃/异常',
      rateLimited: '限流中 (429)',
      overloaded: '过载中 (529)',
      tempUnschedulable: '临时停调度',

      byPlatformTitle: '按平台分布',
      byGroupTitle: '按分组分布',
      platform: '平台',
      group: '分组',
      ungrouped: '未分组',
      total: '总数',
      available: '可调度',

      blockedTitle: '当前不可调度的账号',
      blockedHint: '按恢复时间升序排列。未标注恢复时间的需要人工处理。',
      blockedEmpty: '当前没有被闸门拦截的账号',
      account: '账号',
      reason: '原因',
      recoversIn: '恢复时间',
      needsManualAction: '需人工处理',

      reasonInactive: '状态异常',
      reasonManual: '手动停用',
      reasonRateLimit: '限流中 (429)',
      reasonOverload: '过载中 (529)',
      reasonTempUnsched: '临时停调度',

      slowTitle: '变慢但没报错的账号',
      slowHint:
        '这些账号硬闸门全绿、也不报错，传统的可用性视角完全看不到它们，但用户体感最差。倍率 = 该账号 TTFT P50 ÷ 同模型各账号 P50 的中位数。',
      slowEmpty: '当前没有明显变慢的账号',
      ttftP50: 'TTFT P50',
      ttftBaseline: '同模型基线',
      ttftRatio: '相对倍率',
      ttftWorstModel: '最慢模型',
      ttftPenalized: '已扣健康分',
      ttftWouldPenalize: '达退化阈值（扣分未开启）',
    },

    // ---------- Tab 2 渠道账号 ----------
    accounts: {
      searchPlaceholder: '搜索账号名称…',
      filterPlatform: '平台',
      filterGroup: '分组',
      filterStatus: '调度状态',
      statusAll: '全部',
      statusAvailable: '可调度',
      statusBlocked: '不可调度',

      colAccount: '账号',
      colPlatform: '平台',
      colGroups: '分组',
      colGate: '闸门状态',
      colHealth: '健康分',
      colTtft: '首 Token',
      colPriority: '优先级',
      colLoad: '负载',
      colRuntime: '运行时',
      colLastUsed: '最近使用',
      colActions: '操作',

      gateAvailable: '可调度',
      modelRateLimited: '429 · {model} · {countdown}',
      healthDisabled: '未开启',
      healthTier0: '主池',
      healthTier1: '候选池',
      healthTier2: '隔离观察',
      healthTier0Hint: '正常参与调度',
      healthTier1Hint: '仅当主池账号全部不可用或繁忙时使用',
      healthTier2Hint: '接近熔断，仅当其余账号全部不可用时使用',
      healthSoftSignalHint: '健康分是软信号，只影响排序，不会把账号移出候选集',

      concurrency: '并发',
      rpm: 'RPM',
      windowCost: '窗口费用',
      sessions: '会话',

      ttftRatioTip: '相对同模型基线 {ratio}×',
      ttftWorstTip: '最慢作用域：分组 #{group} · {model}（{ratio}×）',
      ttftNoBaseline: '同模型账号太少，无基线',

      empty: '没有符合条件的账号',
      editAccount: '编辑账号',
      editAccountHint: '凭据、代理等非调度配置请前往账号管理',
    },

    // ---------- Tab 4 调度设置 ----------
    settings: {
      savedSuccess: '保存成功',
      saveFailed: '保存失败',
      save: '保存',
      saving: '保存中…',
      reload: '重新加载',

      healthTitle: '账号健康分',
      healthHint:
        '健康分由上游错误与成功事件驱动（0–100，按半衰期向 100 恢复），只改变账号在候选集内的排序。阈值、扣分权重、半衰期等数值参数目前仅能在 config.yaml 中配置。',
      healthScoringEnabled: '启用健康分',
      healthScoringEnabledHint: '总开关：采集 + 排序',
      healthShadowMode: '影子模式',
      healthShadowModeHint: '只采集并记录分数，不影响选号排序。上线前建议先观察一段时间。',
      healthStickyBreak: '跌入隔离观察层时打破粘性',
      healthStickyBreakHint: '打破粘性会丢失上下文缓存，只在账号接近熔断时才触发',

      ttftTitle: '首 Token 时延（TTFT）',
      ttftHint:
        '健康分原有的扣分触发器全是失败类事件，账号「慢但不报错」时分数永远是 100。开启后，后台任务按「账号 × 模型」聚合 TTFT，与同模型其余账号的基线（各账号 P50 的中位数）比较，明显变慢的账号扣健康分、自然降到候选池。判据是相对倍率而非绝对阈值，避免误杀 thinking / 长上下文这类天然慢的模型。阈值、样本量、巡检周期等数值参数在 config.yaml 中配置。',
      ttftMonitorEnabled: '启用 TTFT 巡检',
      ttftMonitorEnabledHint: '聚合并展示各账号的 TTFT，不影响调度。全程离线，不增加请求耗时。',
      ttftDegradeEnabled: '退化账号扣健康分',
      ttftDegradeEnabledHint: '开启后会改变选号顺序。建议先只开巡检观察一到两周，确认基线稳定再打开。',
      ttftNeedsHealthScoring: '扣分依赖健康分体系：健康分总开关未开启时，TTFT 扣分不会生效。',

      priceTitle: '价格感知调度',
      priceHint: '开启后，同一健康层与优先级内按「价格 × 负载」综合分选择账号。权重与负载守卫阈值在 config.yaml 中配置。',
      priceAwareEnabled: '启用价格感知调度',

      openaiTitle: 'OpenAI 高级调度权重',
      openaiHint:
        'OpenAI 路径使用独立的加权评分模型，与上面的健康分体系是两套并行机制。「生效值」为后端实际使用的值。',
      openaiAdvancedEnabled: '启用高级调度',
      openaiStickyWeighted: '粘性加权',
      openaiSubscriptionPriority: '订阅账号优先',
      openaiLbTopK: '负载均衡候选数 (top-K)',
      weightPriority: '优先级权重',
      weightLoad: '负载权重',
      weightQueue: '队列权重',
      weightErrorRate: '错误率权重',
      weightTtft: '首 Token 时延权重',
      weightReset: '重置临近权重',
      weightQuotaHeadroom: '配额余量权重',
      weightUpstreamCost: '上游成本权重',
      weightPreviousResponse: '上下文亲和权重',
      weightSessionSticky: '会话粘性权重',
      effectiveValue: '生效值',

      cooldownTitle: '冷却与闸门',
      cooldownHint: '这些是硬闸门：命中后账号会被移出候选集，直到冷却结束。',
      cooldown429: '429 限流冷却',
      cooldown529: '529 过载冷却',
      streamTimeout: '流超时处理',
      tempUnschedGuard: '禁止临时停止调度',
      tempUnschedGuardHint: '开启后，网关不再因上游错误自动临时停调度 Anthropic / OpenAI 账号',
      configuredIn: '在系统设置中配置',
      openInSettings: '前往配置',

      otherTitle: '其他调度配置',
      allowUngroupedKey: '允许未分组 API Key 调度',
      allowUngroupedKeyHint: '关闭时，未分组的 Key 直接返回 403',
      errorHandlingRules: '错误处理规则',
      errorHandlingRulesHint: '按上游错误特征自动执行停调度、换号等动作',
      openErrorRules: '前往错误处理规则',

      configOnlyNotice:
        '带此标记的参数当前只能在 config.yaml 中修改，改动后需要重启服务。后续会支持在此页面直接覆盖。',
      configOnly: '仅 config.yaml',
    },
  },
}
