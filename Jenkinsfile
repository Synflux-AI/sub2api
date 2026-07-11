// =============================================================================
// Sub2API CI/CD — Jenkins Declarative Pipeline (Multibranch)
// -----------------------------------------------------------------------------
// - CI gate (unit tests + lint + security scan) runs in ephemeral Docker
//   containers. Each stage's combined output + exit code is captured under
//   ci-logs/ so failures can be reported back to the PR.
// - On every build, post/refresh:
//     * per-stage GitHub commit statuses  (context ci/<stage>)
//     * a sticky PR comment with a per-stage table and, on failure, the tail
//       of each failing stage's log  (see tools/ci/pr_report.py)
//   so a reviewer sees WHICH stage failed and WHY, directly on the PR.
// - On a v* tag: build & push the image to GHCR (amd64). CD is push-only.
// =============================================================================

pipeline {
  agent any

  options {
    disableConcurrentBuilds()
    buildDiscarder(logRotator(numToKeepStr: '30', artifactNumToKeepStr: '10'))
    timestamps()
    timeout(time: 60, unit: 'MINUTES')
  }

  environment {
    REPO          = 'Synflux-AI/sub2api'
    GHCR_IMAGE    = 'ghcr.io/synflux-ai/sub2api'
    GO_IMAGE      = 'golang:1.26.5-alpine'
    GO_FULL_IMAGE = 'golang:1.26.5'
    NODE_IMAGE    = 'node:20-alpine'
    LINT_IMAGE    = 'golangci/golangci-lint:v2.9-alpine'
    PY_IMAGE      = 'python:3-slim'
    GOPROXY       = 'https://goproxy.cn,direct'
    GOSUMDB       = 'sum.golang.google.cn'
    FE_CRITICAL   = 'src/views/auth/__tests__/LinuxDoCallbackView.spec.ts src/views/auth/__tests__/WechatCallbackView.spec.ts src/views/user/__tests__/PaymentView.spec.ts src/views/user/__tests__/PaymentResultView.spec.ts src/components/user/profile/__tests__/ProfileInfoCard.spec.ts src/views/admin/__tests__/SettingsView.spec.ts'
  }

  stages {
    stage('Prepare') {
      steps {
        script {
          sh 'rm -rf ci-logs && mkdir -p ci-logs'
          sh 'git fetch --tags --quiet || true'
          env.RELEASE_TAG = sh(returnStdout: true,
            script: "git tag --points-at HEAD | grep -E '^v[0-9]' | head -n1 || true").trim()
          // Resolve the sha GitHub knows, so per-stage statuses attach to the
          // right commit. For PR (merge) builds that is the PR head sha.
          if (env.CHANGE_ID) {
            withCredentials([usernamePassword(credentialsId: 'github-sub2api-token',
                usernameVariable: 'GH_U', passwordVariable: 'GH_T')]) {
              env.STATUS_SHA = sh(returnStdout: true,
                script: 'curl -sf -H "Authorization: token $GH_T" https://api.github.com/repos/' + env.REPO + '/pulls/' + env.CHANGE_ID + ' | docker run --rm -i "$PY_IMAGE" python -c \'import sys,json;print(json.load(sys.stdin)["head"]["sha"])\'').trim()
            }
          } else {
            env.STATUS_SHA = env.GIT_COMMIT ?: ''
          }
          echo "RELEASE_TAG='${env.RELEASE_TAG}' CHANGE_ID='${env.CHANGE_ID ?: ''}' STATUS_SHA='${env.STATUS_SHA}'"
          // 采集发版信息(changelog/贡献者/tagger/tag说明)供飞书卡片使用；仅 tag 构建需要，全程容错。
          if (env.RELEASE_TAG?.trim()) { computeReleaseInfo(env.RELEASE_TAG) }
        }
      }
    }

    stage('CI') {
      parallel {
        stage('Backend unit tests') {
          steps { ciStage('backend-unit', '''docker run --rm -v "$WORKSPACE":/w -w /w/backend -v jenkins-sub2api-gomod:/go/pkg/mod -v jenkins-sub2api-gocache:/root/.cache/go-build -e GOPROXY -e GOSUMDB "$GO_IMAGE" sh -c 'apk add --no-cache make >/dev/null && make test-unit' ''') }
        }
        stage('golangci-lint') {
          steps { ciStage('golangci-lint', '''docker run --rm -v "$WORKSPACE":/w -w /w/backend -v jenkins-sub2api-gomod:/go/pkg/mod -v jenkins-sub2api-gocache:/root/.cache/go-build -v jenkins-sub2api-golangci:/root/.cache/golangci-lint -e GOPROXY -e GOSUMDB "$LINT_IMAGE" golangci-lint run --timeout=30m''') }
        }
        stage('govulncheck') {
          steps { ciStage('govulncheck', '''docker run --rm -v "$WORKSPACE":/w -w /w/backend -v jenkins-sub2api-gomod:/go/pkg/mod -v jenkins-sub2api-gocache:/root/.cache/go-build -v jenkins-sub2api-gobin:/go/bin -e GOPROXY -e GOSUMDB "$GO_FULL_IMAGE" sh -c 'go install golang.org/x/vuln/cmd/govulncheck@latest && govulncheck ./...' ''') }
        }
        // Frontend lint/typecheck/test + audit share one pnpm install (kept in a
        // single stage on purpose: running two `pnpm install` in parallel against
        // the same store/node_modules races and corrupts the pnpm store,
        // ERR_PNPM_ENOENT). Once install is done, lint/typecheck/vitest/audit are
        // independent read-only checks against the same node_modules, so they run
        // as background jobs and are waited on together instead of chained
        // sequentially — this is the dominant cost of the whole pipeline
        // (~136s serialized: eslint ~56s + vue-tsc ~67s + vitest ~13s), so running
        // them concurrently cuts it to roughly the slowest of the three.
        stage('Frontend') {
          steps { ciStage('frontend', '''docker run --rm -v "$WORKSPACE":/w -w /w/frontend -v jenkins-sub2api-pnpm:/pnpm-store "$NODE_IMAGE" sh -c 'set -e; corepack enable; corepack prepare pnpm@9 --activate; pnpm config set store-dir /pnpm-store; pnpm install --frozen-lockfile; set +e; pnpm run lint:check >lint.log 2>&1 & p_lint=$!; pnpm run typecheck >typecheck.log 2>&1 & p_tsc=$!; pnpm exec vitest run '"$FE_CRITICAL"' >vitest.log 2>&1 & p_vitest=$!; pnpm audit --prod --audit-level=high --json >audit.json 2>audit.log & p_audit=$!; wait $p_lint; rc_lint=$?; wait $p_tsc; rc_tsc=$?; wait $p_vitest; rc_vitest=$?; wait $p_audit; echo "--- lint:check ---"; cat lint.log; echo "--- typecheck ---"; cat typecheck.log; echo "--- vitest ---"; cat vitest.log; echo "--- audit ---"; cat audit.log; [ $rc_lint -eq 0 ] && [ $rc_tsc -eq 0 ] && [ $rc_vitest -eq 0 ]' && docker run --rm -v "$WORKSPACE":/w -w /w "$PY_IMAGE" python tools/check_pnpm_audit_exceptions.py --audit frontend/audit.json --exceptions .github/audit-exceptions.yml''') }
        }
      }
    }

    stage('Build & Push image') {
      when { expression { return env.RELEASE_TAG?.trim() } }
      steps {
        script {
          def version = env.RELEASE_TAG.replaceFirst(/^v/, '')
          def commit  = sh(returnStdout: true, script: 'git rev-parse --short HEAD').trim()
          def date    = sh(returnStdout: true, script: 'date -u +%Y-%m-%dT%H:%M:%SZ').trim()
          withCredentials([usernamePassword(credentialsId: 'ghcr-registry',
              usernameVariable: 'REG_USER', passwordVariable: 'REG_PASS')]) {
            sh """
              echo "\$REG_PASS" | docker login ghcr.io -u "\$REG_USER" --password-stdin
              docker build --build-arg VERSION=${version} --build-arg COMMIT=${commit} --build-arg DATE=${date} -t ${GHCR_IMAGE}:${version} -t ${GHCR_IMAGE}:latest -f Dockerfile .
              docker push ${GHCR_IMAGE}:${version}
              docker push ${GHCR_IMAGE}:latest
              docker logout ghcr.io || true
            """
          }
          echo "Pushed ${GHCR_IMAGE}:${version} and :latest"
        }
      }
    }
  }

  post {
    always  { script { publishReport() } }
    success { script { notifyFeishu('success') } }
    failure { script { notifyFeishu('failure') } }
    aborted { script { notifyFeishu('aborted') } }
  }
}

// Run one CI stage: stream + capture its output to ci-logs/<key>.log, record
// pass/fail in ci-logs/<key>.status, and fail the stage on non-zero exit.
def ciStage(String key, String dockerCmd) {
  // NOTE: Jenkins runs `sh` with `set -e`; without `set +e` a failing docker
  // run would abort the group before the exit code is recorded, masking
  // failures. Capture the real exit code to a file, stream via tee, re-exit.
  def wrapped = 'set +e\n' +
                '{ ' + dockerCmd + ' ; echo $? > ci-logs/' + key + '.rc ; } 2>&1 | tee ci-logs/' + key + '.log\n' +
                'exit $(cat ci-logs/' + key + '.rc)'
  int rc = sh(returnStatus: true, label: key, script: wrapped)
  writeFile file: 'ci-logs/' + key + '.status', text: (rc == 0 ? 'pass' : 'fail')
  if (rc != 0) { error("CI stage '${key}' failed (exit ${rc})") }
}

// Report per-stage statuses + sticky PR comment via GitHub API. Never fails the build.
def publishReport() {
  if (!fileExists('tools/ci/pr_report.py')) { return }
  withCredentials([usernamePassword(credentialsId: 'github-sub2api-token',
      usernameVariable: 'GH_U', passwordVariable: 'GH_T')]) {
    withEnv(["RESULT=${currentBuild.currentResult}",
             "GH_REPO=${env.REPO}",
             "STATUS_SHA=${env.STATUS_SHA ?: ''}",
             "CHANGE_ID=${env.CHANGE_ID ?: ''}",
             "CI_LOGS_DIR=ci-logs"]) {
      sh label: 'pr-report', script: 'docker run --rm -e GH_TOKEN="$GH_T" -e GH_REPO -e CHANGE_ID -e STATUS_SHA -e CI_LOGS_DIR -e BUILD_URL -e BUILD_NUMBER -e RESULT -v "$WORKSPACE":/w -w /w "$PY_IMAGE" python tools/ci/pr_report.py || true'
    }
  }
}

// 飞书交互卡片通知(移植自主仓 linkyrouter Jenkinsfile 的发版卡片，适配 sub2api：
// 单镜像、多分支 PR/branch 上下文、轻量 tag)。tag 构建=完整发版卡片(说明+分组changelog
// +贡献者+版本跨度+镜像)；PR/branch 构建=精简状态卡片。字段拿不到就跳过该行，绝不失败。
def notifyFeishu(String result) {
  def colorMap = [
    success:  'green',
    failure:  'red',
    aborted:  'grey',
    unstable: 'orange',
  ]
  def emojiMap = [
    success:  '✅',
    failure:  '❌',
    aborted:  '🚫',
    unstable: '⚠️',
  ]
  def color = colorMap.get(result, 'blue')
  def emoji = emojiMap.get(result, '🔔')

  def isRelease = env.RELEASE_TAG?.trim() as boolean
  def ref = isRelease ? env.RELEASE_TAG
          : (env.CHANGE_ID ? "PR #${env.CHANGE_ID}" : (env.BRANCH_NAME ?: 'build'))

  // 状态措辞按场景区分：tag 构建=发版(含构建并推送镜像)；PR/branch 构建=CI 检查。
  // 让标题一眼看出"成功的是什么"，而不是笼统的成功/失败。
  def status
  if (result == 'success')       status = isRelease ? '发版成功' : 'CI 通过'
  else if (result == 'failure')  status = isRelease ? '发版失败' : 'CI 失败'
  else if (result == 'aborted')  status = '已中断'
  else if (result == 'unstable') status = 'CI 不稳定'
  else                           status = result

  def title = "sub2api  ${ref}  ·  ${status} ${emoji}"
  def url = env.BUILD_URL ?: ''

  // 触发来源(GitHub push / 手动 / SCM 轮询)；沙箱拿不到就留空
  def trigger = ''
  try {
    trigger = currentBuild.getBuildCauses().collect { it.shortDescription }.findAll { it }.join('；')
  } catch (ignored) { trigger = '' }

  def elements = []
  if (isRelease) {
    // 📌 发版说明(annotated tag 的说明)；轻量 tag 拿到的常是 Merge 提交标题，无意义则跳过
    if (env.TAG_NOTE && !(env.TAG_NOTE =~ /^Merge\s/)) {
      elements << [tag: 'div', text: [tag: 'lark_md', content: "📌 **发版说明**\n${env.TAG_NOTE}"]]
      elements << [tag: 'hr']
    }
    // 改动内容(按 commit 类型分组的两列分栏)
    def changelogEls = buildChangelogElements(env.RELEASE_TAG, env.PREV_TAG, 15)
    if (changelogEls) {
      elements.addAll(changelogEls)
      elements << [tag: 'hr']
    }
  }

  // 元信息:拿不到的字段整行跳过
  def meta = []
  if (isRelease) {
    if (env.TAGGER) meta << "👤 **发版人(tag)**: ${env.TAGGER}"
    if (trigger)    meta << "🔔 **触发**: ${trigger}"
    meta << "🏷️ **版本**: ${env.PREV_TAG ? env.PREV_TAG + ' → ' : ''}**${env.RELEASE_TAG}**"
    meta << "📦 **镜像**: sub2api `:${env.RELEASE_TAG.replaceFirst(/^v/, '')}`  ·  Build #${env.BUILD_NUMBER}"
  } else {
    if (env.CHANGE_ID) {
      def prTitle = env.CHANGE_TITLE ? ": ${env.CHANGE_TITLE}" : ''
      meta << "🔀 **PR**: #${env.CHANGE_ID}${prTitle}"
      def author = env.CHANGE_AUTHOR_DISPLAY_NAME ?: env.CHANGE_AUTHOR
      if (author) {
        def login = (env.CHANGE_AUTHOR && env.CHANGE_AUTHOR != author) ? " (@${env.CHANGE_AUTHOR})" : ''
        meta << "👤 **提交人**: ${author}${login}"
      }
      if (env.CHANGE_BRANCH) meta << "🌿 **分支**: ${env.CHANGE_BRANCH}${env.CHANGE_TARGET ? ' → ' + env.CHANGE_TARGET : ''}"
    } else {
      if (env.BRANCH_NAME) meta << "🌿 **分支**: ${env.BRANCH_NAME}"
      def author = sh(returnStdout: true,
        script: "git log -1 --pretty=format:'%an' 2>/dev/null || true").trim()
      def commit = sh(returnStdout: true,
        script: "git log -1 --pretty=format:'%h  %s' 2>/dev/null || true").trim()
      if (author) meta << "👤 **提交人**: ${author}"
      if (commit) meta << "📝 **提交**: ${commit}"
    }
    if (trigger) meta << "🔔 **触发**: ${trigger}"
    meta << "🧱 **构建**: #${env.BUILD_NUMBER}"
  }
  elements << [tag: 'div', text: [tag: 'lark_md', content: meta.join('\n')]]

  // 失败详情:哪些 CI 阶段挂了 + 首个失败阶段日志尾部(GitHub PR 评论里有完整日志)
  if (result == 'failure' || result == 'unstable') {
    def fd = failureDetail()
    if (fd) {
      elements << [tag: 'hr']
      elements << [tag: 'div', text: [tag: 'lark_md', content: fd]]
    }
  }

  if (url) {
    elements << [tag: 'action', actions: [[tag: 'button', text: [tag: 'plain_text', content: 'Open in Jenkins'], type: 'primary', url: url]]]
  }

  try {
    withCredentials([string(credentialsId: 'sub2api-feishu-webhook', variable: 'FEISHU_HOOK')]) {
      def payload = groovy.json.JsonOutput.toJson([
        msg_type: 'interactive',
        card: [
          config: [wide_screen_mode: true],
          header: [template: color, title: [tag: 'plain_text', content: title]],
          elements: elements
        ]
      ])
      writeFile file: '.feishu-payload.json', text: payload
      sh(label: 'feishu notify',
         script: 'curl -sS -m 10 -H "Content-Type: application/json" --data-binary @.feishu-payload.json "$FEISHU_HOOK" || true; rm -f .feishu-payload.json')
    }
  } catch (ignored) {
    echo 'Feishu webhook credential not configured; skipping notification.'
  }
}

// 从 ci-logs/*.status 找出失败的 CI 阶段(ciStage 每阶段写 pass/fail)，附首个失败阶段的
// 日志尾部。构建/推送镜像阶段失败时没有 .status 文件，返回空串,由卡片头部+Jenkins 链接兜底。
def failureDetail() {
  def fails = sh(returnStdout: true,
    script: "grep -l fail ci-logs/*.status 2>/dev/null | sed 's#.*/##; s#\\.status\$##' || true").trim()
  if (!fails) return ''
  def keys = fails.readLines()
  def out = "❌ **失败阶段**: ${keys.join(', ')}"
  def first = keys[0]
  // 去掉反引号防破坏代码块围栏，每行截断 200 字符
  def tail = sh(returnStdout: true,
    script: "tail -n 6 ci-logs/${first}.log 2>/dev/null | cut -c1-200 | sed 's/`//g' || true").trim()
  if (tail) out += "\n\n**${first} 末尾输出**:\n```\n${tail}\n```"
  return out
}

// 采集发版信息并写入 env（供 notifyFeishu 使用）。全程容错，任何一步失败都只是留空、不中断构建。
def computeReleaseInfo(String tag) {
  // Jenkins checkout 可能是 shallow / 没抓全 tag，先补全，否则 git log 区间和 tagger 拿不到
  sh(label: 'fetch history+tags', script: '''
    set +e
    if [ "$(git rev-parse --is-shallow-repository 2>/dev/null)" = "true" ]; then
      git fetch --unshallow --tags --force >/dev/null 2>&1
    else
      git fetch --tags --force >/dev/null 2>&1
    fi
    true
  ''')

  def prev = sh(returnStdout: true,
    script: "git describe --tags --abbrev=0 '${tag}^' 2>/dev/null || true").trim()
  env.PREV_TAG = prev

  // 发版人(tagger) + 发版说明：annotated tag 才有，lightweight tag 则为空
  // (不采集贡献者：本仓从公开上游 fork 合并后发版，作者列表全是上游噪音)
  env.TAGGER = sh(returnStdout: true,
    script: "git for-each-ref 'refs/tags/${tag}' --format='%(taggername)' 2>/dev/null || true").trim()
  def note = sh(returnStdout: true,
    script: "git for-each-ref 'refs/tags/${tag}' --format='%(contents:subject)' 2>/dev/null || true").trim()
  env.TAG_NOTE = stripVersionPrefix(note)
}

// 去掉 tag 说明里冗余的版本号前缀，如 "v0.1.24: xxx" / "0.1.24：xxx" -> "xxx"（头部已显示版本）
def stripVersionPrefix(String s) {
  if (!s) return ''
  return s.replaceFirst(/^\s*[vV]?\d[\w.\-]*\s*[:：]\s*/, '').trim()
}

// 把区间内的 commit 按类型(feat/fix/perf)分组成飞书两列分栏元素：
//   左栏(72px)= 组标题 + 各 commit 短 hash(加粗)，标题与 hash 同列左对齐
//   右栏(weighted)= 占位空行 + 各 commit 消息，逐行与左栏 hash 对齐
// 其余类型(docs/chore/ci/...)与 Merge 折叠成一句“+N 项杂项”；每组超 cap 条折叠“还有 N 条”。
def buildChangelogElements(String tag, String prev, int cap) {
  if (!tag) return []
  def range = prev ? "${prev}..${tag}" : tag
  def raw = sh(returnStdout: true,
    script: "git log --no-merges --pretty=format:'%s%x1f%h' ${range} 2>/dev/null || true").trim()
  if (!raw) return []
  def sections = [
    [key: 'feat', title: '✨ 功能'],
    [key: 'fix',  title: '🐛 修复'],
    [key: 'perf', title: '⚡ 优化'],
  ]
  def bucket = [feat: [], fix: [], perf: []]
  int misc = 0
  raw.readLines().each { line ->
    int i = line.indexOf('')
    if (i < 0) return
    def subj = line.substring(0, i)
    def hash = line.substring(i + 1)
    def mm = (subj =~ /^(\w+)(?:\([^)]*\))?!?[:：]/)
    def type = mm.find() ? mm.group(1).toLowerCase() : ''
    if (bucket.containsKey(type)) {
      def clean = subj.replaceFirst(/^\w+(?:\([^)]*\))?!?[:：]\s*/, '')
      bucket[type] << [hash: hash, msg: clean]
    } else {
      misc++
    }
  }
  def els = []
  sections.each { s ->
    def items = bucket[s.key]
    if (!items) return
    def shown = items.take(cap)
    def extra = items.size() - shown.size()
    def leftLines  = ["**${s.title}**"] + shown.collect { "**${it.hash}**" }
    def rightLines = ['　']          + shown.collect { it.msg }
    if (extra > 0) { leftLines << '　'; rightLines << "…还有 ${extra} 条" }
    els << [
      tag: 'column_set', flex_mode: 'none',
      columns: [
        [tag: 'column', width: '72px', vertical_align: 'top',
         elements: [[tag: 'div', text: [tag: 'lark_md', content: leftLines.join('\n')]]]],
        [tag: 'column', width: 'weighted', weight: 1, vertical_align: 'top',
         elements: [[tag: 'div', text: [tag: 'lark_md', content: rightLines.join('\n')]]]],
      ]
    ]
  }
  if (misc > 0) {
    els << [tag: 'div', text: [tag: 'lark_md', content: "+ ${misc} 项杂项(docs·chore·ci 等)"]]
  }
  return els
}
