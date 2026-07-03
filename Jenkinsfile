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
    GO_IMAGE      = 'golang:1.26.4-alpine'
    GO_FULL_IMAGE = 'golang:1.26.4'
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

def notifyFeishu(String result) {
  def ref = env.RELEASE_TAG?.trim() ? "tag ${env.RELEASE_TAG}" : (env.BRANCH_NAME ?: 'release')
  def pushed = env.RELEASE_TAG?.trim() ? "\\n镜像: ${env.GHCR_IMAGE}:${env.RELEASE_TAG.replaceFirst(/^v/, '')}" : ''
  def emoji = result == 'success' ? '✅' : '❌'
  def title = "${emoji} Sub2API CI/CD ${result == 'success' ? '成功' : '失败'}"
  def text  = "${title}\\n引用: ${ref}\\n构建: #${env.BUILD_NUMBER}${pushed}\\n${env.BUILD_URL}"
  try {
    withCredentials([string(credentialsId: 'sub2api-feishu-webhook', variable: 'FEISHU_URL')]) {
      sh """
        curl -s -X POST "\$FEISHU_URL" -H 'Content-Type: application/json' -d '{"msg_type":"text","content":{"text":"${text}"}}' || true
      """
    }
  } catch (ignored) {
    echo 'Feishu webhook credential not configured; skipping notification.'
  }
}
